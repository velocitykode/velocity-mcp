package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/velocitykode/velocity/validation"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// ErrValidation is returned by Request.Validate when one or more arguments fail
// their rules. The concrete *validation.ValidationErrors (carrying per-field
// messages) is wrapped; recover it with errors.As. Methods turn this into a
// tool-level error result rather than a transport error.
var ErrValidation = errors.New("mcp: request validation failed")

// ErrRuleField is returned by Request.Validate when a rule names a field the
// validation engine and the argument accessors do not resolve to the same
// value, which the engine's narrower addressing makes possible (see
// ruleFieldFault). Running such a rule would report a check the handler did not
// get: the value it then reads is one no rule inspected. The fault is in the
// server's own rules rather than in the arguments, so this is not a validation
// failure and does not wrap ErrValidation; the handler surfaces it like any
// other error.
var ErrRuleField = errors.New("mcp: validation rule field is not addressable")

// Request carries the arguments of a tool, resource, or prompt invocation: a
// typed view over the decoded "arguments" object plus session metadata. The
// zero value is not usable; the server constructs Requests from incoming
// JSON-RPC params.
//
// Typed getters come in two forms: the plain form (String, Int, Float, Bool)
// returns the zero value when the key is missing or the wrong type, and the
// ok-variant (StringOK, ...) additionally reports whether a usable value was
// present. This mirrors Go's comma-ok idiom.
type Request struct {
	args      map[string]any
	sessionID string
	meta      map[string]any
	uri       string
	emit      func(msg []byte) error

	// uriVars names the arguments a resource template bound out of the concrete
	// uri. The server derived those values from the uri it resolved, so they are
	// read under exactly the name the template declared and never as a path (see
	// Request.lookup). Nil for every request that is not a templated read.
	uriVars map[string]struct{}

	// ctx is the inbound request context threaded from the transport. It backs
	// User, which reads the authenticated identity off the serving router
	// context the HTTP transport stores on it. Never nil after NewRequest.
	ctx context.Context
}

// ProgressUpdate is a single progress report for a long-running tool or
// resource handler, sent to the client as a notifications/progress message. The
// client only receives it when it supplied a progressToken in the request
// _meta and the serving transport streams (HTTP with an event-stream Accept, or
// stdio); otherwise ReportProgress is a no-op.
type ProgressUpdate struct {
	// Progress is the amount of work done so far. It should increase across
	// successive reports for the same request.
	Progress float64
	// Total is the total amount of work expected, when known. A value <= 0 is
	// omitted from the wire so the client treats progress as indeterminate.
	Total float64
	// Message is an optional human-readable status string for this step.
	Message string
}

// NewRequest builds a Request from a decoded arguments map. A nil map is
// treated as empty. It is primarily used by the method handlers and tests.
func NewRequest(args map[string]any) *Request {
	if args == nil {
		args = map[string]any{}
	}
	return &Request{args: args, ctx: context.Background()}
}

// WithRequestContext returns the request with the transport's inbound request
// context set. The method handlers wire it from the serving server Context so
// User can reach the request-scoped auth state; an empty ctx defaults to
// context.Background(), keeping User nil-safe.
func (r *Request) WithRequestContext(ctx context.Context) *Request {
	if ctx == nil {
		ctx = context.Background()
	}
	r.ctx = ctx
	return r
}

// WithSessionID returns the request with its session id set.
func (r *Request) WithSessionID(id string) *Request {
	r.sessionID = id
	return r
}

// WithMeta returns the request with its _meta map set.
func (r *Request) WithMeta(meta map[string]any) *Request {
	r.meta = meta
	return r
}

// WithURI returns the request with its concrete resource uri set (used by
// resources/read).
func (r *Request) WithURI(uri string) *Request {
	r.uri = uri
	return r
}

// WithURIVariables merges the variables a resource template matched out of the
// concrete uri into the arguments, and records their names as uri-bound. Those
// names are then read under exactly the name the template declared: a variable
// name may carry a dot (RFC 6570 section 2.3), and resolving it as a path would
// let caller-supplied arguments spelling that path answer in its place. An
// empty map is a no-op.
func (r *Request) WithURIVariables(vars map[string]string) *Request {
	if len(vars) == 0 {
		return r
	}
	values := make(map[string]any, len(vars))
	if r.uriVars == nil {
		r.uriVars = make(map[string]struct{}, len(vars))
	}
	for name, value := range vars {
		values[name] = value
		r.uriVars[name] = struct{}{}
	}
	r.merge(values)
	return r
}

// WithEmitter installs the sink used to send progress notifications back to the
// client during a streaming request. The method handlers wire it from the
// serving Context; a nil emitter (the non-streaming case) leaves ReportProgress
// a no-op.
func (r *Request) WithEmitter(emit func(msg []byte) error) *Request {
	r.emit = emit
	return r
}

// ReportProgress sends a progress notification for this request. It is a no-op
// (returning nil) unless the client supplied a progressToken in the request
// _meta and the transport provided a streaming sink, so handlers can call it
// unconditionally. A non-nil error is the sink's write failure; handlers may
// ignore it, since progress is best-effort and never affects the final result.
func (r *Request) ReportProgress(p ProgressUpdate) error {
	if r.emit == nil {
		return nil
	}
	token, ok := r.meta["progressToken"]
	if !ok || token == nil {
		return nil
	}
	params := map[string]any{
		"progressToken": token,
		"progress":      p.Progress,
	}
	if p.Total > 0 {
		params["total"] = p.Total
	}
	if p.Message != "" {
		params["message"] = p.Message
	}
	n, err := jsonrpc.NewNotification("notifications/progress", params)
	if err != nil {
		return err
	}
	msg, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return r.emit(msg)
}

// SessionID returns the id of the session that issued the request, or "".
func (r *Request) SessionID() string { return r.sessionID }

// Meta returns the request's _meta map, or nil when none was supplied.
func (r *Request) Meta() map[string]any { return r.meta }

// URI returns the concrete resource uri for a resources/read request, or "".
func (r *Request) URI() string { return r.uri }

// All returns a shallow copy of every argument keyed by name.
func (r *Request) All() map[string]any {
	out := make(map[string]any, len(r.args))
	for k, v := range r.args {
		out[k] = v
	}
	return out
}

// Has reports whether the named argument is present (even if null). key is an
// argument name, or a dot path into nested arguments (see Get). It answers
// exactly when Get finds a value, so an argument Has reports is always
// readable.
func (r *Request) Has(key string) bool {
	return r.has(key)
}

// Get returns the raw argument value for key, or nil when absent. key is an
// argument name, or a dot path addressing a value inside a nested argument
// ("user.name", "items.0", "items.*.id"). Dots are separators and a "*" segment
// is a wildcard; a key that addresses nothing that way is read as the plain
// name of a top-level argument, so a property an inputSchema declares as
// "limit.items" is read as it arrived. The path wins when both spellings
// arrive, so what a handler reads for a path never depends on a peer adding a
// property whose name spells it; Arg reads the exact name. A variable a
// resource template bound out of the uri is itself always read under its exact
// name, so no caller-supplied argument can answer in its place.
//
// A key that passed its rules reads back the value those rules saw, because
// Validate refuses a rule field it would not resolve the same way (see
// Request.lookup).
func (r *Request) Get(key string) any {
	v, _ := r.lookup(key)
	return v
}

// Arg returns the argument declared with exactly this name, and whether it was
// present. The name is never read as a path: dots and "*" are part of it, which
// is how a property an inputSchema declares as "limit.items" is read when the
// arguments also carry a nested "limit" object, and how a resource template
// variable named "user.id" (legal under RFC 6570 section 2.3) is read once the
// read has merged it into the arguments. A rule field is a path and cannot
// address such a property, so a value read here is one Validate never checked
// and the handler checks itself.
func (r *Request) Arg(name string) (any, bool) {
	v, ok := r.args[name]
	return v, ok
}

// merge adds the given values into the request arguments, used by templated
// resource reads to inject extracted URI variables. A nil map is a no-op.
func (r *Request) merge(values map[string]any) {
	if len(values) == 0 {
		return
	}
	if r.args == nil {
		r.args = make(map[string]any, len(values))
	}
	for k, v := range values {
		r.args[k] = v
	}
}

// StringOK returns the named argument as a string and whether it was present
// and string-typed. key may be a dot path (see Get).
func (r *Request) StringOK(key string) (string, bool) {
	v, ok := r.lookup(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// String returns the named argument as a string, or "" when absent or not a
// string.
func (r *Request) String(key string) string {
	s, _ := r.StringOK(key)
	return s
}

// FloatOK returns the named argument as a float64 and whether it was present
// and numeric. JSON numbers decode to float64; an integer-valued json.Number or
// other numeric forms are also accepted. key may be a dot path (see Get).
func (r *Request) FloatOK(key string) (float64, bool) {
	v, ok := r.lookup(key)
	if !ok {
		return 0, false
	}
	return toFloat(v)
}

// Float returns the named argument as a float64, or 0 when absent or not
// numeric.
func (r *Request) Float(key string) float64 {
	f, _ := r.FloatOK(key)
	return f
}

// IntOK returns the named argument as an int64 and whether it was present and
// numeric with a whole value. A fractional number reports ok=false.
func (r *Request) IntOK(key string) (int64, bool) {
	f, ok := r.FloatOK(key)
	if !ok {
		return 0, false
	}
	if f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

// Int returns the named argument as an int64, or 0 when absent or not a whole
// number.
func (r *Request) Int(key string) int64 {
	n, _ := r.IntOK(key)
	return n
}

// BoolOK returns the named argument as a bool and whether it was present and
// boolean-typed. key may be a dot path (see Get).
func (r *Request) BoolOK(key string) (bool, bool) {
	v, ok := r.lookup(key)
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// Bool returns the named argument as a bool, or false when absent or not a
// bool.
func (r *Request) Bool(key string) bool {
	b, _ := r.BoolOK(key)
	return b
}

// Bind JSON round-trips the request arguments into dst, which must be a
// non-nil pointer, hydrating typed arguments. A decode error is returned to the
// caller (the method handler surfaces it as an error result, never leaking
// internals to clients).
func (r *Request) Bind(dst any) error {
	b, err := json.Marshal(r.args)
	if err != nil {
		return fmt.Errorf("mcp: encode arguments: %w", err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("mcp: bind arguments: %w", err)
	}
	return nil
}

// Validate checks the request arguments against the given velocity validation
// rules using the framework validation engine (never validation/dbrules, so the
// ORM stays out of the import graph). On failure it returns an error that wraps
// ErrValidation and the concrete *validation.ValidationErrors; on success it
// returns nil.
//
// A rule field is a dot path into nested argument objects. The engine walks
// nested objects only, so it reaches neither an array element ("items.0"), nor
// the elements a "*" segment collects, nor an argument whose own name spells
// the field, while the accessors reach all three. A field where that difference
// shows is refused with an error wrapping ErrRuleField before any rule runs, so
// a field whose rules passed always reads back the value those rules saw;
// collections and arguments read by their own name are checked by the handler
// itself, after reading them.
//
// The guarantee covers the field a rule is written for. A rule that names a
// second field as a parameter (Same, RequiredIf and their kind) resolves that
// name through the validation engine, on the engine's own terms.
func (r *Request) Validate(rules validation.Rules) error {
	// Fields are checked in a stable order: a rule set with more than one
	// faulty field must name the same one every run.
	fields := make([]string, 0, len(rules))
	for field := range rules {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		if fault := r.ruleFieldFault(field); fault != "" {
			return fmt.Errorf("%w: %q: %s", ErrRuleField, field, fault)
		}
	}

	v := validation.NewValidator()
	if _, err := v.Validate(r.args, rules); err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	return nil
}

// toFloat coerces a decoded JSON numeric value to float64. It accepts float64
// (the default JSON number type), json.Number, and the integer kinds in case
// arguments were constructed programmatically rather than decoded from JSON.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
