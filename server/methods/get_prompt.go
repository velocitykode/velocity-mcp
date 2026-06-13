package methods

import (
	"errors"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// GetPrompt handles "prompts/get": it resolves the named prompt, runs it against
// the request arguments, and serializes the messages.
//
// A missing "name" or an unknown prompt is an InvalidParams (-32602) protocol
// error. A validation failure becomes an error result text prefixed with
// "Invalid params: ", surfaced as a single user message.
type GetPrompt struct{}

var _ server.Method = GetPrompt{}

// Handle resolves and invokes the requested prompt.
func (GetPrompt) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)

	name := p.str("name")
	if !p.has("name") || name == "" {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Missing [name] parameter."), nil
	}

	prompt := findPrompt(c, name)
	if prompt == nil {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Prompt ["+name+"] not found."), nil
	}

	request := server.NewRequest(p.arguments()).
		WithSessionID(c.SessionID()).
		WithMeta(p.mapValue("_meta")).
		WithEmitter(c.Emit)

	resp, err := prompt.Handle(c.RequestContext(), request)
	if err != nil {
		if errors.Is(err, server.ErrValidation) {
			resp = server.Error("Invalid params: " + validationMessage(err))
		} else {
			return nil, err
		}
	}

	result, serr := promptResult(prompt.Description(), resp)
	if serr != nil {
		result, _ = promptResult(prompt.Description(), server.Error("The prompt returned content that cannot be represented in a prompt message."))
	}
	return jsonrpc.NewResult(req.ID, result)
}

// findPrompt returns the registered prompt with the given name, or nil.
func findPrompt(c *server.Context, name string) server.Prompt {
	for _, p := range c.Prompts() {
		if p.Name() == name {
			return p
		}
	}
	return nil
}
