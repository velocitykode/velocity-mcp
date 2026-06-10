package methods

import (
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// CompletionComplete handles "completion/complete": argument autocompletion for
// a prompt or resource reference.
//
// Completion providers (the Completable primitives) are out of scope for this
// phase, so a resolvable but non-completable primitive returns an empty
// completion. The capability gate, parameter validation, and reference
// resolution still apply: completions must be advertised (else MethodNotFound),
// and a missing ref or argument is InvalidParams.
type CompletionComplete struct{}

var _ server.Method = CompletionComplete{}

// Handle validates the completion request and returns an empty completion set.
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

	// The primitive is resolved, then an empty completion is returned for any
	// primitive that is not Completable BEFORE inspecting argument.name. No
	// primitive in this phase implements Completable, so every resolvable
	// reference is non-completable and yields the empty completion shape here,
	// even when argument.name is absent. This ordering avoids a spurious
	// "Missing argument name." InvalidParams for a non-completable reference.
	return jsonrpc.NewResult(req.ID, map[string]any{
		"completion": emptyCompletion(),
	})
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
