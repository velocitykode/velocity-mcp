package methods

import (
	"errors"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// ReadResource handles "resources/read": it resolves the resource for the
// requested uri (matching templates where needed), reads it, and serializes the
// contents.
//
// A missing or unresolvable uri is a ResourceNotFound (-32002) protocol error.
// A validation failure becomes an error result text prefixed with "Invalid
// params: ".
type ReadResource struct{}

var _ server.Method = ReadResource{}

// Handle resolves and reads the requested resource.
func (ReadResource) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)

	uri := p.str("uri")
	if !p.has("uri") || uri == "" {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeResourceNotFound, "Missing [uri] parameter."), nil
	}

	resource, vars := resolveResource(c, uri)
	if resource == nil {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeResourceNotFound, "Resource ["+uri+"] not found."), nil
	}

	args := p.arguments()
	for k, v := range vars {
		args[k] = v
	}
	request := server.NewRequest(args).
		WithSessionID(c.SessionID()).
		WithMeta(p.mapValue("_meta")).
		WithURI(uri).
		WithEmitter(c.Emit)

	resp, err := resource.Read(c.RequestContext(), request)
	if err != nil {
		if errors.Is(err, server.ErrValidation) {
			resp = server.Error("Invalid params: " + validationMessage(err))
		} else {
			return nil, err
		}
	}

	result, serr := resourceResult(uri, resource.MimeType(), resp)
	if serr != nil {
		result, _ = resourceResult(uri, resource.MimeType(), server.Error("The resource returned content that cannot be represented in a resource read."))
	}
	return jsonrpc.NewResult(req.ID, result)
}

// resolveResource finds the resource matching uri: first an exact match on a
// registered non-template resource, then a template match. It returns the
// resolved resource and any variables extracted from a template match (nil for
// a plain resource).
func resolveResource(c *server.Context, uri string) (server.Resource, map[string]string) {
	for _, r := range c.Resources() {
		if r.URI() == uri {
			return r, nil
		}
	}
	for _, t := range c.ResourceTemplates() {
		if t.URITemplate() == uri {
			return t, nil
		}
		if vars, ok := server.MatchURITemplate(t.URITemplate(), uri); ok {
			return t, vars
		}
	}
	return nil, nil
}
