package methods

import (
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// CompletionComplete handles "completion/complete": argument autocompletion for
// a prompt or resource reference.
//
// A prompt or resource supplies completions by implementing server.Completable.
// A resolvable reference whose primitive is not Completable returns an empty
// completion. The capability gate, parameter validation, and reference
// resolution always apply: completions must be advertised (else MethodNotFound),
// and a missing ref or argument is InvalidParams.
type CompletionComplete struct{}

var _ server.Method = CompletionComplete{}

// Handle validates the completion request, invokes the referenced primitive's
// completion provider when it implements server.Completable, and returns the
// resulting completion set (empty for a non-completable reference).
func (CompletionComplete) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	if !c.HasCapability(server.CapabilityCompletions) {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeMethodNotFound, "Server does not support completions capability."), nil
	}

	p := decode(req)
	ref := p.mapValue("ref")
	argument := p.mapValue("argument")
	if ref == nil || argument == nil {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Missing required parameters: ref and argument"), nil
	}

	if err := validateReference(c, ref); err != nil {
		return jsonrpc.NewErrorResponse(req.ID, err), nil
	}

	// A non-completable (but resolvable) reference yields the empty completion
	// shape without inspecting argument.name, so a missing name is not a
	// spurious InvalidParams for primitives that offer no completions.
	comp := completableFor(c, ref)
	if comp == nil {
		return jsonrpc.NewResult(req.ID, map[string]any{"completion": emptyCompletion()})
	}

	name, _ := argument["name"].(string)
	if name == "" {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Missing [name] parameter."), nil
	}
	value, _ := argument["value"].(string)

	result, err := comp.Complete(c.RequestContext(), server.CompletionRequest{
		Argument: name,
		Value:    value,
		Context:  completionContext(p),
	})
	if err != nil {
		// A provider failure must not leak internal detail to the client.
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInternalError, "Completion failed."), nil
	}

	return jsonrpc.NewResult(req.ID, map[string]any{
		"completion": completionShape(result),
	})
}

// completableFor resolves the primitive named by ref and returns it as a
// server.Completable, or nil when the reference is not completable.
func completableFor(c *server.Context, ref map[string]any) server.Completable {
	switch ref["type"] {
	case "ref/prompt":
		name, _ := ref["name"].(string)
		if pr := findPrompt(c, name); pr != nil {
			if comp, ok := pr.(server.Completable); ok {
				return comp
			}
		}
	case "ref/resource":
		uri, _ := ref["uri"].(string)
		if r, _ := resolveResource(c, uri); r != nil {
			if comp, ok := r.(server.Completable); ok {
				return comp
			}
		}
	}
	return nil
}

// completionContext extracts the optional completion context: sibling argument
// values the client has already resolved, under params.context.arguments. Only
// string values are kept; a missing or malformed context yields nil.
func completionContext(p params) map[string]string {
	ctx := p.mapValue("context")
	if ctx == nil {
		return nil
	}
	args, ok := ctx["arguments"].(map[string]any)
	if !ok || len(args) == 0 {
		return nil
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// completionShape renders a server.Completion to its completion/complete wire
// shape. Total defaults to the number of values when unset, and values is
// always a non-nil slice so it serializes as [] rather than null.
func completionShape(comp server.Completion) map[string]any {
	values := comp.Values
	if values == nil {
		values = []string{}
	}
	total := comp.Total
	if total < len(values) {
		total = len(values)
	}
	return map[string]any{
		"values":  values,
		"total":   total,
		"hasMore": comp.HasMore,
	}
}

// validateReference resolves a completion reference (ref/prompt or ref/resource)
// to ensure it names a registered primitive, returning a *jsonrpc.Error when the
// reference type is unknown or the primitive cannot be found.
func validateReference(c *server.Context, ref map[string]any) *jsonrpc.Error {
	switch ref["type"] {
	case "ref/prompt":
		name, _ := ref["name"].(string)
		if name == "" {
			return jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Missing [name] parameter.")
		}
		if findPrompt(c, name) == nil {
			return jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Prompt ["+name+"] not found.")
		}
		return nil
	case "ref/resource":
		uri, _ := ref["uri"].(string)
		if uri == "" {
			return jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Missing [uri] parameter.")
		}
		if r, _ := resolveResource(c, uri); r == nil {
			return jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Resource ["+uri+"] not found.")
		}
		return nil
	default:
		return jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Invalid reference type. Expected ref/prompt or ref/resource.")
	}
}

// emptyCompletion returns the empty completion result shape.
func emptyCompletion() map[string]any {
	return map[string]any{
		"values":  []string{},
		"total":   0,
		"hasMore": false,
	}
}
