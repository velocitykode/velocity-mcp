package methods

import (
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// Discover handles "server/discover", the opening exchange of the discovery
// handshake. It answers with the protocol versions the server speaks, the
// capabilities it advertises, and its instructions; the server implementation
// metadata travels in the result _meta the server attaches to every result, so
// it is not repeated in the body.
//
// Unlike initialize, discovery negotiates nothing and establishes no session:
// the client picks a version from "supportedVersions" and restates it in the
// params._meta of every subsequent request.
type Discover struct{}

// Compile-time assertion that the handler satisfies server.Method.
var _ server.Method = Discover{}

// Handle returns the server's discovery document.
func (Discover) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, map[string]any{
		"supportedVersions": c.SupportedProtocolVersions(),
		"capabilities":      c.Capabilities(),
		"instructions":      c.Instructions(),
	})
}
