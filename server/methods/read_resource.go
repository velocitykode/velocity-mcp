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
// A missing uri is InvalidParams (-32602) under every revision: a required
// parameter that is absent is a malformed request, not a resource that could
// not be found. A uri that resolves to no registered resource is a protocol
// error whose code follows the revision the request is made under: revision
// 2026-07-28 reports InvalidParams (-32602), because the uri is a request
// parameter the server could not make sense of, while the revisions that
// predate it report ResourceNotFound (-32002), the code their resource
// specification names and the one their clients read to tell a resource that is
// not there from parameters they got wrong. A validation failure becomes an
// error result text prefixed with "Invalid params: ".
//
// A result that reports a failure as content (a validation failure, content the
// read form cannot represent, or an error response the resource itself
// returned) carries the zero caching hint whatever the resource or the
// operation is configured with: resource results have no error marker, so a
// client or shared cache storing one under the resource's own lifetime and
// audience would serve the failure as the resource, to every caller, for as
// long as the resource itself would have been fresh. The caching hints are
// required to describe the result actually returned, and a failure is not the
// resource.
type ReadResource struct{}

var _ server.Method = ReadResource{}

// Handle resolves and reads the requested resource.
func (ReadResource) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)

	uri := p.str("uri")
	if !p.has("uri") || uri == "" {
		return jsonrpc.NewErrorResponseCode(req.ID, jsonrpc.CodeInvalidParams, "Missing [uri] parameter."), nil
	}

	resource, vars := resolveResource(c, uri)
	if resource == nil {
		return jsonrpc.NewErrorResponseCode(req.ID, unresolvedResourceCode(req), "Resource ["+uri+"] not found."), nil
	}

	request := server.NewRequest(p.arguments()).
		WithSessionID(c.SessionID()).
		WithMeta(p.mapValue("_meta")).
		WithURI(uri).
		WithURIVariables(vars).
		WithEmitter(c.Emit).
		WithRequestContext(c.RequestContext())

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
		resp = server.Error("The resource returned content that cannot be represented in a resource read.")
		result, _ = resourceResult(uri, resource.MimeType(), resp)
	}
	if resp.IsError() {
		server.CacheHint{}.Apply(result)
	}
	return jsonrpc.NewResult(req.ID, result)
}

// unresolvedResourceCode returns the protocol error code a resources/read
// naming no readable resource is answered with, under the revision the request
// declares. The discovery revision reports the uri as a parameter the server
// could not make sense of; every revision before it reports ResourceNotFound,
// which is the distinction its clients draw between a resource that is not
// there and parameters they got wrong, so the code they were written against is
// the one they keep.
func unresolvedResourceCode(req *jsonrpc.Request) int {
	version, _ := server.RequestProtocolVersion(req)
	if server.HandshakeFor(version) == server.HandshakeDiscovery {
		return jsonrpc.CodeInvalidParams
	}
	return jsonrpc.CodeResourceNotFound
}

// resolveResource finds the resource matching uri: first an exact match on a
// registered non-template resource, then a template match. It returns the
// resolved resource and any variables extracted from a template match (nil for
// a plain resource). The resolution itself lives on the context, so the read
// and the caching hints the result carries address the same resource.
func resolveResource(c *server.Context, uri string) (server.Resource, map[string]string) {
	return c.ResolveResource(uri)
}
