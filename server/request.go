package server

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/velocitykode/velocity/validation"
)

// ErrValidation is returned by Request.Validate when one or more arguments fail
// their rules. The concrete *validation.ValidationErrors (carrying per-field
// messages) is wrapped; recover it with errors.As. Methods turn this into a
// tool-level error result rather than a transport error.
var ErrValidation = errors.New("mcp: request validation failed")

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
}

// NewRequest builds a Request from a decoded arguments map. A nil map is
// treated as empty. It is primarily used by the method handlers and tests.
func NewRequest(args map[string]any) *Request {
	if args == nil {
		args = map[string]any{}
	}
	return &Request{args: args}
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

// Has reports whether the named argument is present (even if null).
func (r *Request) Has(key string) bool {
	_, ok := r.args[key]
	return ok
}

// Get returns the raw argument value for key, or nil when absent.
func (r *Request) Get(key string) any { return r.args[key] }

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
// and string-typed.
func (r *Request) StringOK(key string) (string, bool) {
	v, ok := r.args[key]
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
// other numeric forms are also accepted.
func (r *Request) FloatOK(key string) (float64, bool) {
	v, ok := r.args[key]
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
// boolean-typed.
func (r *Request) BoolOK(key string) (bool, bool) {
	v, ok := r.args[key]
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
func (r *Request) Validate(rules validation.Rules) error {
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
