package server

import "github.com/velocitykode/velocity-mcp/jsonrpc"

// initializeMethod is a minimal, self-contained initialize handler used as a
// fallback when the methods package has not been imported (so no method factory
// is installed). It performs the same capability negotiation as methods.Initialize
// so a server that only imports the server package can still complete the
// handshake. When the methods package is imported its richer Initialize replaces
// this via the factory.
type initializeMethod struct{}

// Handle negotiates the protocol version and returns the initialize result. An
// unsupported requested version yields an InvalidParams error carrying the
// supported and requested versions, mirroring laravel/mcp's Initialize.
func (initializeMethod) Handle(c *Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return Initialize(c, req)
}

// Initialize is the shared initialize negotiation used by both the fallback and
// the methods package, so the two never drift. It mirrors laravel/mcp's
// Server\Methods\Initialize exactly: an explicit unsupported version is an
// InvalidParams (-32602) error with {supported, requested} data; otherwise the
// negotiated version is the requested one (when supported) or the server's
// first supported version.
func Initialize(c *Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	params := decodeParams(req.Params)
	supported := c.SupportedProtocolVersions()

	requested, hasRequested := params["protocolVersion"].(string)
	if hasRequested && !containsVersion(supported, requested) {
		return jsonrpc.NewErrorResponse(req.ID,
			jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Unsupported protocol version").
				WithData(map[string]any{
					"supported": supported,
					"requested": requested,
				}),
		), nil
	}

	negotiated := requested
	if !hasRequested {
		if len(supported) > 0 {
			negotiated = supported[0]
		}
	}
	c.SetNegotiatedVersion(negotiated)

	result := map[string]any{
		"protocolVersion": negotiated,
		"capabilities":    c.Capabilities(),
		"serverInfo":      c.Implementation().ToMap(),
		"instructions":    c.Instructions(),
	}
	return jsonrpc.NewResult(req.ID, result)
}

// containsVersion reports whether v is in the list.
func containsVersion(list []ProtocolVersion, v ProtocolVersion) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
