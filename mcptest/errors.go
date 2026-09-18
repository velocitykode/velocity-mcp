package mcptest

// This file exposes the reply's error set and the explicit has-errors /
// has-no-errors pair. A reply carries errors in one of two shapes: a
// protocol-level JSON-RPC error object, or a successful result flagged
// isError:true whose content items hold the message (the MCP tool-level error
// result). Both shapes read the same way here.

// Errors returns the reply's error messages: the JSON-RPC error message for a
// protocol-level failure, or the content items of a tool-level error result.
// An untroubled reply yields an empty slice, and so does a failing reply that
// carries no message, an error result being free to flag itself without
// content; the empty slice is therefore not a statement that the reply
// succeeded. AssertHasErrors / AssertHasNoErrors answer that question.
//
// The caller owns the returned slice: errors() builds it from the reply on every
// call rather than handing out stored state, so writing into it disturbs neither
// the reply nor another reader.
func (r *Response) Errors() []string {
	if errs := r.errors(); errs != nil {
		return errs
	}
	return []string{}
}

// AssertHasNoErrors asserts the reply carries neither a protocol-level error
// object nor a tool-level isError result. AssertOk is the shorthand for the same
// check.
func (r *Response) AssertHasNoErrors() *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.AssertOk()
}

// AssertHasErrors asserts the reply carries at least one error, and that each of
// the given messages appears as a substring of some error message. AssertError
// is the shorthand for the same check.
func (r *Response) AssertHasErrors(messages ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.AssertError(messages...)
}
