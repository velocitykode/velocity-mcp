package server

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/velocitykode/velocity/contract"
)

// internalErrorMessage is the client-facing wording for a failure the server
// cannot describe without leaking internal detail. It is the message a
// "tools/call" internal error response carries, so a failure contained inside a
// batch reads the same as the same failure on the direct path.
const internalErrorMessage = "Something went wrong while processing the request."

// unrepresentableContentMessage is returned when a handler produced content
// that has no shape in a tool result (for example a blob, which is valid only
// in a resource read).
const unrepresentableContentMessage = "The tool returned content that cannot be represented in a tool result."

// ErrNoTool is returned by InvokeTool when it is handed no tool to run. It is a
// caller defect rather than a client one, so it never reaches the client: the
// protocol maps it to the same generic internal error any other unexpected
// failure produces.
var ErrNoTool = errors.New("mcp: no tool to invoke")

// InvokeTool runs a resolved tool against req and builds the "tools/call"
// result defined by the MCP specification: a "content" array of per-item tool
// shapes, an "isError" flag, and any merged _meta and structured content.
//
// It is the single invocation path: both the "tools/call" method handler and
// the tool catalog's batch execution go through it, so argument validation,
// error mapping and result serialization can never drift between a direct call
// and a call made through the catalog.
//
// Error handling mirrors the protocol split between a tool-level error result
// and a protocol-level failure. A validation failure (an error wrapping
// ErrValidation) is client-facing and becomes an error result with the field
// messages, paired with a nil error. Content that cannot be represented in a
// tool result likewise becomes an error result. Any other handler failure is
// returned unchanged for the caller to map, so no internal detail can reach the
// client by accident.
//
// A nil tool is reported as a failed call rather than dereferenced: a caller
// that resolved nothing gets an error to map, never a panic. A nil request is
// run as a request with no arguments, which a tool validating its arguments
// rejects on its own terms.
func InvokeTool(ctx context.Context, tool Tool, req *Request) (map[string]any, error) {
	if tool == nil {
		return nil, ErrNoTool
	}
	if req == nil {
		req = NewRequest(nil)
	}

	resp, err := tool.Handle(ctx, req)
	if err != nil {
		if !errors.Is(err, ErrValidation) {
			return nil, err
		}
		resp = Error(ValidationMessage(err))
	}

	result, serr := ToolResult(resp)
	if serr != nil {
		result, _ = ToolResult(Error(unrepresentableContentMessage))
	}
	return result, nil
}

// ToolResult builds the "tools/call" result map from a response: the per-item
// content shapes, the isError flag, and any merged _meta and structured
// content. It returns an error when a content item has no tool shape; callers
// surface that as a tool-level error result rather than a failed call.
func ToolResult(resp *Response) (map[string]any, error) {
	if resp == nil {
		return map[string]any{"content": []any{}, "isError": false}, nil
	}

	items := make([]any, 0, len(resp.Contents()))
	for _, c := range resp.Contents() {
		shape, err := c.ToTool()
		if err != nil {
			return nil, err
		}
		items = append(items, shape)
	}

	return resp.mergeMeta(map[string]any{
		"content": items,
		"isError": resp.IsError(),
	}), nil
}

// ValidationMessage renders a validation error into a single client-facing
// string: every field message, joined in sorted field order so the message is
// deterministic. When no field message can be recovered a generic fallback is
// returned, so no internal detail leaks.
func ValidationMessage(err error) string {
	var verr contract.ValidationErrors
	if errors.As(err, &verr) && len(verr.Errors) > 0 {
		fields := make([]string, 0, len(verr.Errors))
		for field := range verr.Errors {
			fields = append(fields, field)
		}
		sort.Strings(fields)

		var messages []string
		for _, field := range fields {
			messages = append(messages, verr.Errors[field]...)
		}
		if len(messages) > 0 {
			return strings.Join(messages, " ")
		}
	}
	return "The given data was invalid."
}
