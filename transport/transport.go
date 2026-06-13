package transport

import (
	"context"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"

	// Blank-import the protocol method set so it self-registers with the server
	// package. The MCP methods (tools/list, tools/call, resources/*, prompts/*,
	// etc.) are mandatory protocol surface, not opt-in, and live in their own
	// package because they import server (which therefore cannot import them
	// back). Every serving path goes through a transport, so importing it here
	// guarantees the methods are registered for any consumer that serves a
	// server, without requiring a magic blank import in their own main package.
	_ "github.com/velocitykode/velocity-mcp/server/methods"
)

// Transport is the contract every MCP transport implements, shaped to Go
// idioms: a blocking Run loop driven by a context for cancellation, and a Send
// that emits an already-encoded message frame. Implementations are responsible
// for framing (line-delimited for stdio, the HTTP body for the HTTP transport).
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

// streamingServer is the optional extension a server implements to support
// streamed, server-initiated frames (such as notifications/progress) sent
// before the final result. *server.Server satisfies it; a server that does not
// is driven through the one-shot Handle. It is an optional interface so the
// MCPServer contract (and existing stub servers) stay minimal.
type streamingServer interface {
	HandleStream(ctx context.Context, raw []byte, sessionID string, emit func(msg []byte) error) server.HandleResult
}

// handleMessage routes one raw inbound message through srv. When emit is non-nil
// and srv supports streaming, the streaming entry point is used so the handler
// can send intermediate frames through emit; otherwise the one-shot Handle is
// used. The final HandleResult is returned in both cases.
func handleMessage(srv MCPServer, ctx context.Context, raw []byte, sessionID string, emit func(msg []byte) error) server.HandleResult {
	if emit != nil {
		if ss, ok := srv.(streamingServer); ok {
			return ss.HandleStream(ctx, raw, sessionID, emit)
		}
	}
	return srv.Handle(ctx, raw, sessionID)
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
