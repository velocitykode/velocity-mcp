package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
	_ "github.com/velocitykode/velocity-mcp/server/methods" // installs the protocol method set
)

// stubServer is a minimal MCPServer used to drive transport behaviour
// deterministically without standing up a full *server.Server. It returns a
// scripted HandleResult per call and records the session id it was handed. It
// is concurrency-safe so it can back the Fake's concurrent tests.
type stubServer struct {
	fn func(ctx context.Context, raw []byte, sessionID string) server.HandleResult

	mu            sync.Mutex
	lastSessionID string
	calls         int
}

func (s *stubServer) Handle(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
	s.mu.Lock()
	s.lastSessionID = sessionID
	s.calls++
	s.mu.Unlock()
	return s.fn(ctx, raw, sessionID)
}

func (s *stubServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// mustResult builds a success response for the given id or fails the test.
func mustResult(t *testing.T, id jsonrpc.ID, result any) *jsonrpc.Response {
	t.Helper()
	resp, err := jsonrpc.NewResult(id, result)
	if err != nil {
		t.Fatalf("NewResult: %v", err)
	}
	return resp
}

// newTestServer builds a real server with a single "add" tool so end-to-end
// transport tests exercise the genuine Handle path (initialize + tools/call).
func newTestServer(t *testing.T) *server.Server {
	t.Helper()
	add := server.NewTool("add", "Add two numbers").
		WithSchema(func(s *schema.Object) {
			s.Number("a").Required()
			s.Number("b").Required()
		}).
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return server.Text(fmt.Sprintf("%v", req.Float("a")+req.Float("b"))), nil
		})
	srv := server.New("test", "1.0.0", server.WithTools(add))
	srv.SetSessionIDGenerator(func() string { return "fixed-session" })
	return srv
}

// Compile-time checks that the concrete transports satisfy the contract.
var (
	_ Transport = (*Stdio)(nil)
	_ Transport = (*Fake)(nil)
)

func TestEncodeResponse(t *testing.T) {
	tests := []struct {
		name    string
		resp    *jsonrpc.Response
		wantNil bool
	}{
		{name: "nil response yields nil frame", resp: nil, wantNil: true},
		{name: "success response marshals", resp: mustResult(t, jsonrpc.IntID(1), map[string]any{"ok": true})},
		{name: "error response marshals", resp: jsonrpc.NewErrorResponseCode(jsonrpc.IntID(2), jsonrpc.CodeMethodNotFound, "nope")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeResponse(tt.resp)
			if err != nil {
				t.Fatalf("encodeResponse: %v", err)
			}
			if tt.wantNil {
				if got != nil {
					t.Fatalf("want nil frame, got %q", got)
				}
				return
			}
			if !json.Valid(got) {
				t.Fatalf("frame is not valid JSON: %q", got)
			}
		})
	}
}

// initializeRequest returns a raw initialize message.
func initializeRequest(id int64) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, id))
}

// callToolRequest returns a raw tools/call message for "add".
func callToolRequest(id int64, a, b float64) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"add","arguments":{"a":%v,"b":%v}}}`, id, a, b))
}

// decodeResponse unmarshals a raw frame into a jsonrpc.Response.
func decodeResponse(t *testing.T, raw []byte) jsonrpc.Response {
	t.Helper()
	var resp jsonrpc.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return resp
}
