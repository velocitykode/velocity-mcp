package methods

import (
	"errors"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// CallTool handles "tools/call": it resolves the named tool, runs it against the
// request arguments, and serializes the result.
//
// Error handling: a missing "name" or an unknown tool is an
// InvalidParams (-32602) protocol error; a validation failure from the tool
// handler becomes a tool-level error result (isError:true) rather than a
// protocol error, so the client sees the message without the call failing. Any
// other handler error propagates as a Go error, which server.Handle turns into a
// generic internal error response (no internal detail leaks to the client).
type CallTool struct{}

var _ server.Method = CallTool{}

// Handle resolves and invokes the requested tool.
func (CallTool) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)

	name := p.str("name")
	if !p.has("name") || name == "" {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Missing [name] parameter."), nil
	}

	tool := findTool(c, name)
	if tool == nil {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Tool ["+name+"] not found."), nil
	}

	request := server.NewRequest(p.arguments()).
		WithSessionID(c.SessionID()).
		WithMeta(p.mapValue("_meta")).
		WithEmitter(c.Emit)

	resp, err := tool.Handle(c.RequestContext(), request)
	if err != nil {
		// A validation failure is a client-facing error result, not a protocol
		// error, surfaced to the client as a tool-level error response.
		if errors.Is(err, server.ErrValidation) {
			resp = server.Error(validationMessage(err))
		} else {
			// Unexpected failure: propagate so the server returns a generic
			// internal error without leaking detail.
			return nil, err
		}
	}

	result, serr := toolResult(resp)
	if serr != nil {
		// A content type that cannot be represented in a tool result is a
		// client-facing error result, not an internal failure.
		result, _ = toolResult(server.Error("The tool returned content that cannot be represented in a tool result."))
	}
	return jsonrpc.NewResult(req.ID, result)
}

// findTool returns the registered tool with the given name, or nil.
func findTool(c *server.Context, name string) server.Tool {
	for _, t := range c.Tools() {
		if t.Name() == name {
			return t
		}
	}
	return nil
}
