package methods

import (
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// InitializeMethod handles the "initialize" request: it negotiates the protocol
// version and returns the server capabilities, implementation metadata, and
// instructions.
//
// The negotiation itself lives in server.Initialize so the server package's
// fallback handler and this handler never drift. The Server (server.Handle)
// owns the surrounding session assignment and SessionInitialized event dispatch.
type InitializeMethod struct{}

// Compile-time assertion that the handler satisfies server.Method.
var _ server.Method = InitializeMethod{}

// Handle negotiates the protocol version and returns the initialize result.
func (InitializeMethod) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return server.Initialize(c, req)
}

// Ping handles the "ping" request with an empty success result.
type Ping struct{}

// Compile-time assertion that the handler satisfies server.Method.
var _ server.Method = Ping{}

// Handle returns an empty result object.
func (Ping) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, map[string]any{})
}
