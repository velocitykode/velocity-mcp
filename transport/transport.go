package transport

import (
	"context"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// Transport is the contract every MCP transport implements. It mirrors
// laravel/mcp's Server\Contracts\Transport, reshaped to Go idioms: a blocking
// Run loop driven by a context for cancellation, and a Send that emits an
// already-encoded message frame. Implementations are responsible for framing
// (line-delimited for stdio, the HTTP body for the HTTP transport).
type Transport interface {
	// Run drives the transport's message loop until ctx is cancelled or the
	// underlying stream ends. It blocks for the lifetime of the connection and
	// returns nil on a clean stop (EOF or context cancellation) or a non-nil
	// error on an unrecoverable I/O failure.
	Run(ctx context.Context) error

	// Send emits a single already-encoded message frame to the peer. It returns
	// a non-nil error if the frame cannot be written. Send is safe to call from
	// within the Run loop's message handling.
	Send(ctx context.Context, msg []byte) error
}

// MCPServer is the surface a transport needs from an MCP server: the ability to
// handle one raw inbound JSON-RPC message and return what to send back. It is
// declared as an interface (rather than taking a concrete *server.Server
// everywhere) so tests can drive a transport with a stub and the Fake transport
// stays decoupled. *server.Server satisfies it.
type MCPServer interface {
	Handle(ctx context.Context, raw []byte, sessionID string) server.HandleResult
}

// encodeResponse marshals a JSON-RPC response into a single message frame
// (without any trailing newline; framing is the transport's responsibility). A
// nil response yields a nil slice so callers can skip sending.
func encodeResponse(resp *jsonrpc.Response) ([]byte, error) {
	if resp == nil {
		return nil, nil
	}
	return json.Marshal(resp)
}
