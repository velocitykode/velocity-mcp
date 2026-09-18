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
// supported and requested versions.
func (initializeMethod) Handle(c *Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return Initialize(c, req)
}

// Initialize is the shared initialize negotiation used by both the fallback and
// the methods package, so the two never drift.
//
// The initialize handshake is the legacy opening exchange, so it negotiates
// only over InitializeSupportedVersions: a client asking for one of those
// versions is answered with it, and any other request (an unknown version, a
// non-string value, or no version at all) is answered with the newest version
// the handshake offers. The negotiation never fails, because a client that
// cannot speak an offered version simply closes the connection; a client that
// speaks the discovery revision opens with server/discover instead.
func Initialize(c *Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	params := decodeParams(req.Params)
	offered := InitializeSupportedVersions()

	negotiated := offered[0]
	if requested, ok := params["protocolVersion"].(string); ok && containsVersion(offered, requested) {
		negotiated = requested
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
