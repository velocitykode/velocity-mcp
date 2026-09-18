// Package jsonrpc defines the JSON-RPC 2.0 message types (request, response,
// notification, error) used by the MCP protocol. Stdlib-only leaf package.
//
// The wire format follows the JSON-RPC 2.0 specification
// (https://www.jsonrpc.org/specification) as constrained by the Model Context
// Protocol. Batching is intentionally unsupported: the current MCP spec dropped
// JSON-RPC batches, so each message is a single object.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the only JSON-RPC protocol version accepted by MCP.
const Version = "2.0"

// Standard JSON-RPC 2.0 error codes (https://www.jsonrpc.org/specification#error_object).
const (
	// CodeParseError indicates invalid JSON was received by the server.
	CodeParseError = -32700
	// CodeInvalidRequest indicates the JSON sent is not a valid Request object.
	CodeInvalidRequest = -32600
	// CodeMethodNotFound indicates the method does not exist or is not available.
	CodeMethodNotFound = -32601
	// CodeInvalidParams indicates invalid method parameters.
	CodeInvalidParams = -32602
	// CodeInternalError indicates an internal JSON-RPC error.
	CodeInternalError = -32603
)

// MCP-specific error codes. These live in the implementation-defined server
// range (-32000 to -32099) reserved by the JSON-RPC spec and are defined by the
// MCP specification.
const (
	// CodeResourceNotFound indicates the requested resource (URI) could not be
	// resolved. It is what a resources/read is answered with under the
	// revisions preceding 2026-07-28, which report an unresolvable URI this way
	// so a client can tell it from malformed parameters; 2026-07-28 reports it
	// as CodeInvalidParams instead.
	CodeResourceNotFound = -32002
	// CodeHeaderMismatch indicates an MCP HTTP header (MCP-Protocol-Version,
	// Mcp-Method, Mcp-Name) is absent or contradicts the request body.
	CodeHeaderMismatch = -32020
	// CodeMissingRequiredClientCapability indicates the client did not declare a
	// capability the requested operation requires.
	CodeMissingRequiredClientCapability = -32021
	// CodeUnsupportedProtocolVersion indicates the protocol version the request
	// declared is not one the server speaks. The error data carries the
	// "supported" list and the "requested" value.
	CodeUnsupportedProtocolVersion = -32022
)

// ID represents a JSON-RPC request identifier. Per the specification an id MUST
// be a string, a number, or null. The raw JSON is preserved so the exact client
// value (and numeric formatting) is echoed back unchanged in responses.
type ID struct {
	// raw holds the original JSON token for the id. A nil raw means the id was
	// absent (notification) or explicitly null.
	raw json.RawMessage
}

// StringID builds an ID from a string value.
func StringID(s string) ID {
	b, _ := json.Marshal(s)
	return ID{raw: b}
}

// IntID builds an ID from an integer value.
func IntID(n int64) ID {
	b, _ := json.Marshal(n)
	return ID{raw: b}
}

// NullID is the explicit JSON null id, used for error responses that cannot be
// correlated to a request id (per the JSON-RPC spec, error responses to
// unparseable requests carry a null id).
func NullID() ID {
	return ID{raw: json.RawMessage("null")}
}

// IsNull reports whether the id is absent or JSON null.
func (id ID) IsNull() bool {
	if len(id.raw) == 0 {
		return true
	}
	return bytes.Equal(bytes.TrimSpace(id.raw), []byte("null"))
}

// IsValidRequestID reports whether the id is a non-null string or number, the
// only forms permitted for a request id by the specification.
func (id ID) IsValidRequestID() bool {
	if id.IsNull() {
		return false
	}
	t := bytes.TrimSpace(id.raw)
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '"': // string
		return true
	case '-', '+', '.':
		return true
	default:
		return t[0] >= '0' && t[0] <= '9'
	}
}

// String returns the id rendered for human-readable output. A string id is
// returned without its surrounding quotes; numbers are returned as-is; a null
// or absent id yields an empty string.
func (id ID) String() string {
	if id.IsNull() {
		return ""
	}
	var s string
	if err := json.Unmarshal(id.raw, &s); err == nil {
		return s
	}
	return string(bytes.TrimSpace(id.raw))
}

// Raw returns a copy of the underlying JSON token for the id.
func (id ID) Raw() json.RawMessage {
	if len(id.raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(id.raw))
	copy(out, id.raw)
	return out
}

// MarshalJSON implements json.Marshaler. An absent id marshals to null.
func (id ID) MarshalJSON() ([]byte, error) {
	if len(id.raw) == 0 {
		return []byte("null"), nil
	}
	return id.raw, nil
}

// UnmarshalJSON implements json.Unmarshaler, preserving the raw token.
//
// The token is required to be JSON, and an id left over from a previous message
// is dropped rather than kept beside a refused one. Decoding a whole message
// hands this method nothing else, but reading an id straight off untrusted
// bytes is how a transport recovers the id to answer a message it has not
// parsed, and MarshalJSON writes the token back out as it came: a token that is
// not JSON would then fail no earlier than the encoding of the reply that
// carries it, far from the bytes that caused it. An ID that reports no error
// always holds a token its own Marshaler can write.
func (id *ID) UnmarshalJSON(b []byte) error {
	if !json.Valid(b) {
		id.raw = nil
		return errors.New("jsonrpc: id is not a JSON value")
	}
	id.raw = append(id.raw[:0], b...)
	return nil
}

// Error is the JSON-RPC error object carried in an error response.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data is optional, structured additional information. Omitted when nil.
	Data any `json:"data,omitempty"`
}

// Error implements the error interface so a *jsonrpc.Error can be returned and
// inspected with errors.As.
func (e *Error) Error() string {
	if e == nil {
		return "<nil jsonrpc error>"
	}
	return fmt.Sprintf("jsonrpc: code %d: %s", e.Code, e.Message)
}

// NewError builds a *Error with the given code and message.
func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithData returns a copy of the error carrying the supplied data payload.
func (e *Error) WithData(data any) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Data = data
	return &clone
}

// Request is an inbound JSON-RPC request: a call that expects a response and so
// carries an id. Params is left as raw JSON for the method handler to decode.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification is an inbound JSON-RPC notification: a one-way message with no id
// and no response.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is an outbound JSON-RPC response. Exactly one of Result or Error is
// set: Result for success, Error for failure.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// NewResult builds a success Response correlating to id and carrying result.
// A nil result is encoded as an empty JSON object: an empty result is
// represented as {} rather than null.
func NewResult(id ID, result any) (*Response, error) {
	raw, err := marshalResult(result)
	if err != nil {
		return nil, err
	}
	return &Response{JSONRPC: Version, ID: id, Result: raw}, nil
}

// NewErrorResponse builds a failure Response correlating to id and carrying err.
func NewErrorResponse(id ID, err *Error) *Response {
	return &Response{JSONRPC: Version, ID: id, Error: err}
}

// NewErrorResponseCode is a convenience that builds a failure Response from a
// raw code and message.
func NewErrorResponseCode(id ID, code int, message string) *Response {
	return NewErrorResponse(id, NewError(code, message))
}

// marshalResult encodes a result value, substituting an empty object for nil so
// the wire form never carries a null result on success.
func marshalResult(result any) (json.RawMessage, error) {
	if result == nil {
		return json.RawMessage("{}"), nil
	}
	if raw, ok := result.(json.RawMessage); ok {
		if len(bytes.TrimSpace(raw)) == 0 {
			return json.RawMessage("{}"), nil
		}
		out := make(json.RawMessage, len(raw))
		copy(out, raw)
		return out, nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: marshal result: %w", err)
	}
	return b, nil
}

// EncodeNotification encodes an outbound notification straight to the frame a
// transport writes. Building the message and encoding it are one step, so a
// caller that only ever sends the frame answers for a single failure: params a
// peer could not be sent. Splitting the two leaves the second step unable to
// fail, since the value it encodes is one this package has already encoded
// once, and a branch no input can reach is a branch nothing can hold to its
// behaviour.
func EncodeNotification(method string, params any) ([]byte, error) {
	frame, err := json.Marshal(notificationFrame{JSONRPC: Version, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: marshal notification: %w", err)
	}
	return frame, nil
}

// notificationFrame is the wire shape EncodeNotification writes. It holds the
// caller's params value as it came, so the whole frame is encoded in one pass;
// a nil params value is omitted, exactly as Notification omits an empty one.
type notificationFrame struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// NewNotification builds an outbound Notification with the given method and
// params. A nil params value is omitted from the wire form.
func NewNotification(method string, params any) (*Notification, error) {
	n := &Notification{JSONRPC: Version, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("jsonrpc: marshal notification params: %w", err)
		}
		n.Params = raw
	}
	return n, nil
}
