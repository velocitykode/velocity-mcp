package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// errTrailingData is returned by strictUnmarshal when content follows the first
// JSON value (e.g. a second message in the same buffer).
var errTrailingData = errors.New("jsonrpc: trailing data after JSON value")

// Sentinel error messages are the canonical JSON-RPC error strings so the
// wire-level diagnostics are stable and descriptive.
const (
	msgParseError       = "Parse error: Invalid JSON was received by the server."
	msgInvalidID        = "Invalid Request: The [id] member must be a string, number."
	msgInvalidVersion   = "Invalid Request: The [jsonrpc] member must be exactly [2.0]."
	msgMissingMethodReq = "Invalid Request: The [method] member is required and must be a string."
	msgMissingMethodNtf = "Invalid Request: Invalid or missing [method]. Must be a string."
	msgInvalidNtfVer    = "Invalid Request: Invalid JSON-RPC version. Must be [2.0]."
	msgInvalidParams    = "Invalid params: The [params] member must be an object."
	msgResultWithoutID  = "Invalid Response: A [result] must carry the [id] of the request it answers."
)

// envelope is the permissive shape used to inspect any incoming JSON-RPC
// message before deciding whether it is a request or a notification. Members are
// kept as raw JSON keyed by name so presence (an explicit null id) can be
// distinguished from absence: a *json.RawMessage field cannot tell those apart
// because encoding/json decodes a JSON null into a nil pointer either way.
type envelope map[string]json.RawMessage

// has reports whether the named member is present in the message (even if its
// value is JSON null).
func (e envelope) has(key string) bool {
	_, ok := e[key]
	return ok
}

// hasNonNull reports whether the named member is present AND its value is not
// JSON null. A present-but-null member counts as "not set", which is how
// messages are routed: a present-but-null id is treated as absent.
func (e envelope) hasNonNull(key string) bool {
	v, ok := e[key]
	if !ok {
		return false
	}
	return !bytes.Equal(bytes.TrimSpace(v), []byte("null"))
}

// raw returns the raw token for the named member, or nil when absent.
func (e envelope) raw(key string) json.RawMessage {
	if v, ok := e[key]; ok {
		return v
	}
	return nil
}

// IsNotificationBytes reports whether the incoming JSON-RPC message should be
// routed as a notification (no reply) rather than a request. A message is a
// notification when it has no usable id member: either the id is absent or it is
// present but JSON null. A present-but-null id is treated as absent when
// choosing between a request and a notification. Malformed JSON yields a
// CodeParseError.
func IsNotificationBytes(data []byte) (bool, error) {
	var env envelope
	if err := strictUnmarshal(data, &env); err != nil {
		return false, NewError(CodeParseError, msgParseError)
	}
	return !env.hasNonNull("id"), nil
}

// ParseRequest strictly decodes a JSON-RPC request. It rejects malformed JSON
// (CodeParseError), a missing or non-string/number id, a jsonrpc version other
// than exactly "2.0", and a missing or non-string method (all CodeInvalidRequest).
// On the id and version/method checks the recovered id (when usable) is returned
// so callers can correlate the error response.
func ParseRequest(data []byte) (*Request, ID, *Error) {
	var env envelope
	if err := strictUnmarshal(data, &env); err != nil {
		return nil, NullID(), NewError(CodeParseError, msgParseError)
	}

	// Recover the id first so it can be echoed on subsequent validation errors:
	// the id is read before any other member is validated.
	var id ID
	if env.has("id") {
		id = ID{raw: cloneRaw(env.raw("id"))}
	}

	if !id.IsValidRequestID() {
		// Only a usable (string/number) id is echoed; otherwise null.
		return nil, NullID(), NewError(CodeInvalidRequest, msgInvalidID)
	}

	if !versionIs20(env.raw("jsonrpc")) {
		return nil, id, NewError(CodeInvalidRequest, msgInvalidVersion)
	}

	method, ok := stringValue(env.raw("method"))
	if !ok {
		return nil, id, NewError(CodeInvalidRequest, msgMissingMethodReq)
	}

	if env.has("params") && !isParamsObject(env.raw("params")) {
		return nil, id, NewError(CodeInvalidParams, msgInvalidParams)
	}

	return &Request{
		JSONRPC: Version,
		ID:      id,
		Method:  method,
		Params:  cloneRaw(env.raw("params")),
	}, id, nil
}

// ParseNotification strictly decodes a JSON-RPC notification. It rejects
// malformed JSON (CodeParseError), a jsonrpc version other than exactly "2.0",
// and a missing or non-string method (CodeInvalidRequest). A notification
// carries no id.
func ParseNotification(data []byte) (*Notification, *Error) {
	var env envelope
	if err := strictUnmarshal(data, &env); err != nil {
		return nil, NewError(CodeParseError, msgParseError)
	}

	if !versionIs20(env.raw("jsonrpc")) {
		return nil, NewError(CodeInvalidRequest, msgInvalidNtfVer)
	}

	method, ok := stringValue(env.raw("method"))
	if !ok {
		return nil, NewError(CodeInvalidRequest, msgMissingMethodNtf)
	}

	if env.has("params") && !isParamsObject(env.raw("params")) {
		return nil, NewError(CodeInvalidParams, msgInvalidParams)
	}

	return &Notification{
		JSONRPC: Version,
		Method:  method,
		Params:  cloneRaw(env.raw("params")),
	}, nil
}

// ParseResponse strictly decodes a JSON-RPC response (a reply received from a
// peer). It validates the version and that exactly one of result/error is
// present. Malformed JSON yields CodeParseError; a structurally invalid
// response yields CodeInvalidRequest.
func ParseResponse(data []byte) (*Response, *Error) {
	var env envelope
	if err := strictUnmarshal(data, &env); err != nil {
		return nil, NewError(CodeParseError, msgParseError)
	}
	if !versionIs20(env.raw("jsonrpc")) {
		return nil, NewError(CodeInvalidRequest, msgInvalidVersion)
	}

	hasResult := len(bytes.TrimSpace(env.raw("result"))) > 0
	var rpcErr *Error
	if errRaw := env.raw("error"); len(bytes.TrimSpace(errRaw)) > 0 && !bytes.Equal(bytes.TrimSpace(errRaw), []byte("null")) {
		if err := json.Unmarshal(errRaw, &rpcErr); err != nil {
			return nil, NewError(CodeInvalidRequest, "Invalid Response: malformed [error] object.")
		}
	}
	hasError := rpcErr != nil
	if hasResult == hasError {
		// Neither or both present: not a valid response object.
		return nil, NewError(CodeInvalidRequest, "Invalid Response: exactly one of [result] or [error] must be present.")
	}

	// A reply names the call it answers. The specification requires the id of
	// the request on every response and allows it to be absent only where the
	// id could not be read at all, which is to say on a failure correlated to
	// nothing. A result stating none answers no call a caller made, so it is
	// refused here rather than handed on under a null id that a caller matching
	// on ids could take for an uncorrelated reply it may act on.
	if hasResult && !env.has("id") {
		return nil, NewError(CodeInvalidRequest, msgResultWithoutID)
	}

	resp := &Response{JSONRPC: Version, Result: cloneRaw(env.raw("result")), Error: rpcErr}
	if env.has("id") {
		resp.ID = ID{raw: cloneRaw(env.raw("id"))}
	} else {
		resp.ID = NullID()
	}
	return resp, nil
}

// strictUnmarshal decodes data into v, rejecting trailing garbage after the
// first JSON value. It does not disallow unknown fields, since JSON-RPC peers
// may add spec-permitted extension members.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Reject any non-whitespace trailing content, whether it is a second JSON
	// value or a stray token. Decoding again is what settles it: only an input
	// that ends after the first value reports io.EOF. Asking the decoder
	// whether there is More would not do, because it answers no for a trailing
	// "]" or "}" (the tokens that end a value it is not inside), leaving those
	// bodies to parse as if the garbage were not there.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errTrailingData
	}
	return nil
}

// versionIs20 reports whether the jsonrpc member is present and exactly "2.0".
func versionIs20(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	return s == Version
}

// isParamsObject reports whether a present [params] member carries a structured
// value the protocol accepts. The MCP specification models params as an object,
// so a JSON object is the canonical form; an empty JSON array is also accepted
// because some encoders render an empty map that way, and it decodes to the
// same empty parameter set. Any other value (null, a string, a number, a
// non-empty array) is rejected.
func isParamsObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	switch {
	case bytes.HasPrefix(t, []byte("{")):
		return json.Valid(t)
	case bytes.HasPrefix(t, []byte("[")):
		// A lone "[" is not valid JSON, so the slice below is only reached for a
		// token carrying both brackets.
		return json.Valid(t) && len(bytes.TrimSpace(t[1:len(t)-1])) == 0
	default:
		return false
	}
}

// stringValue extracts a non-null JSON string from raw, reporting ok=false when
// the member is absent, null, or any other JSON type.
//
// The token is decoded as a free-form value and then required to be a string.
// Decoding straight into a string would report success for a JSON null, which
// encoding/json leaves the destination untouched for, so a message stating
// "method": null would be read as a request naming the empty method instead of
// the invalid request it is.
func stringValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	s, ok := value.(string)
	return s, ok
}

// cloneRaw returns an independent copy of a raw JSON token, or nil when empty.
func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}
