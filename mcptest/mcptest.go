package mcptest

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/transport"
)

// defaultProtocolVersion is the protocol version mcptest negotiates during
// Initialize. It matches one of the server's supported versions so the handshake
// always succeeds against a default-configured server.
const defaultProtocolVersion = "2025-06-18"

// Server drives a *server.Server through the in-memory Fake transport with
// fluent, t-aware assertions. It mirrors laravel/mcp's
// Server\Testing\PendingTestResponse: each driver method (Initialize, CallTool,
// ListTools, ReadResource, GetPrompt, ...) sends one JSON-RPC message through
// the server and returns a *Response carrying the reply for chained assertions.
//
// A Server is not safe for concurrent use by multiple goroutines: it threads a
// single Fake transport and a monotonically increasing request id. Drive one
// Server per test (or per sequential scenario).
type Server struct {
	t      testing.TB
	srv    *server.Server
	fake   *transport.Fake
	ctx    context.Context
	nextID atomic.Int64
}

// NewServer builds a test harness around srv, wiring it to a fresh Fake
// transport. t is used to fail the test (via t.Helper/t.Fatalf) when an
// assertion does not hold, mirroring laravel/mcp's test-response helpers that
// call PHPUnit assertions. The full MCP method set must be installed (blank
// import _ "github.com/velocitykode/velocity-mcp/server/methods") for list and
// call methods to resolve; an un-imported method surfaces as a MethodNotFound
// response that the assertions report.
//
// NewServer registers a cleanup that drains the Fake transport's recordings so a
// reused server does not leak frames across subtests.
func NewServer(t testing.TB, srv *server.Server) *Server {
	if t != nil {
		t.Helper()
	}
	ts := &Server{
		t:    t,
		srv:  srv,
		fake: transport.NewFake(srv),
		ctx:  context.Background(),
	}
	if t != nil {
		t.Cleanup(ts.fake.Reset)
	}
	return ts
}

// WithContext sets the context threaded into every driven message (so event
// listeners observe request-scoped values). It returns the Server for chaining.
// A nil context is replaced with context.Background().
func (s *Server) WithContext(ctx context.Context) *Server {
	if ctx == nil {
		ctx = context.Background()
	}
	s.ctx = ctx
	return s
}

// SessionID returns the session id assigned by the most recent successful
// Initialize, or "" before the handshake completes.
func (s *Server) SessionID() string {
	return s.fake.SessionID()
}

// Initialize performs the MCP initialize handshake and returns the reply for
// assertions. The negotiated protocol version and client info are fixed values
// suitable for tests; use InitializeWith to override them. A successful
// Initialize assigns a session id that subsequent calls reuse (the Fake
// transport retains it), mirroring how a real client carries Mcp-Session-Id.
func (s *Server) Initialize() *Response {
	return s.InitializeWith(defaultProtocolVersion, "mcptest", "1.0.0")
}

// InitializeWith performs the initialize handshake with an explicit protocol
// version and client implementation name/version. It returns the reply for
// assertions.
func (s *Server) InitializeWith(protocolVersion, clientName, clientVersion string) *Response {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    clientName,
			"version": clientVersion,
		},
	}
	return s.call("initialize", params)
}

// CallTool invokes a tool (tools/call) with the given arguments and returns the
// reply for assertions. A nil arguments map is sent as an empty object. It
// mirrors laravel/mcp's PendingTestResponse::tool.
func (s *Server) CallTool(name string, arguments map[string]any) *Response {
	if arguments == nil {
		arguments = map[string]any{}
	}
	return s.call("tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
}

// ListTools requests the tool catalogue (tools/list) and returns the reply for
// assertions (AssertToolListed, ...).
func (s *Server) ListTools() *Response {
	return s.call("tools/list", map[string]any{})
}

// ListResources requests the resource catalogue (resources/list) and returns
// the reply for assertions (AssertResourceListed, ...).
func (s *Server) ListResources() *Response {
	return s.call("resources/list", map[string]any{})
}

// ListPrompts requests the prompt catalogue (prompts/list) and returns the
// reply for assertions (AssertPromptListed, ...).
func (s *Server) ListPrompts() *Response {
	return s.call("prompts/list", map[string]any{})
}

// ReadResource reads a resource by uri (resources/read) and returns the reply
// for assertions. It mirrors laravel/mcp's PendingTestResponse::resource.
func (s *Server) ReadResource(uri string) *Response {
	return s.call("resources/read", map[string]any{"uri": uri})
}

// GetPrompt renders a prompt (prompts/get) with the given arguments and returns
// the reply for assertions. A nil arguments map is sent as an empty object. It
// mirrors laravel/mcp's PendingTestResponse::prompt.
func (s *Server) GetPrompt(name string, arguments map[string]any) *Response {
	if arguments == nil {
		arguments = map[string]any{}
	}
	return s.call("prompts/get", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
}

// Ping sends a ping request and returns the reply for assertions.
func (s *Server) Ping() *Response {
	return s.call("ping", nil)
}

// Send drives an arbitrary JSON-RPC method with the given params (which may be
// nil) through the server, returning the reply for assertions. It is the escape
// hatch for methods without a dedicated helper.
func (s *Server) Send(method string, params map[string]any) *Response {
	return s.call(method, params)
}

// Notify drives a JSON-RPC notification (a message with no id, expecting no
// reply) through the server, returning a *Response whose AssertNoResponse holds.
// It mirrors a client emitting e.g. notifications/initialized.
func (s *Server) Notify(method string, params map[string]any) *Response {
	if s.t != nil {
		s.t.Helper()
	}
	note := jsonrpc.Notification{JSONRPC: jsonrpc.Version, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			s.fatalf("mcptest: marshal params for notification %q: %v", method, err)
			return newResponse(s.t, method, nil)
		}
		note.Params = raw
	}
	raw, err := json.Marshal(note)
	if err != nil {
		s.fatalf("mcptest: marshal notification %q: %v", method, err)
		return newResponse(s.t, method, nil)
	}
	reply, err := s.fake.Inject(s.ctx, raw)
	if err != nil {
		s.fatalf("mcptest: drive notification %q through fake transport: %v", method, err)
		return newResponse(s.t, method, nil)
	}
	resp, err := decodeResponse(reply)
	if err != nil {
		s.fatalf("mcptest: decode reply for notification %q: %v", method, err)
		return newResponse(s.t, method, nil)
	}
	return newResponse(s.t, method, resp)
}

// call marshals a JSON-RPC request for method/params, drives it through the Fake
// transport, and decodes the reply into a *Response. Marshal/transport failures
// fail the test (they indicate a broken harness, not a server behaviour under
// test).
func (s *Server) call(method string, params map[string]any) *Response {
	if s.t != nil {
		s.t.Helper()
	}

	id := s.nextID.Add(1)
	req := jsonrpc.Request{
		JSONRPC: jsonrpc.Version,
		ID:      jsonrpc.IntID(id),
		Method:  method,
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			s.fatalf("mcptest: marshal params for %q: %v", method, err)
			return newResponse(s.t, method, nil)
		}
		req.Params = raw
	}

	raw, err := json.Marshal(req)
	if err != nil {
		s.fatalf("mcptest: marshal request for %q: %v", method, err)
		return newResponse(s.t, method, nil)
	}

	reply, err := s.fake.Inject(s.ctx, raw)
	if err != nil {
		s.fatalf("mcptest: drive %q through fake transport: %v", method, err)
		return newResponse(s.t, method, nil)
	}

	resp, err := decodeResponse(reply)
	if err != nil {
		s.fatalf("mcptest: decode reply for %q: %v", method, err)
		return newResponse(s.t, method, nil)
	}
	return newResponse(s.t, method, resp)
}

// fatalf reports a harness failure through t when present, falling back to a
// no-op when the harness was built with a nil t (which keeps the helpers usable
// outside a test, e.g. in examples).
func (s *Server) fatalf(format string, args ...any) {
	if s.t == nil {
		return
	}
	s.t.Helper()
	s.t.Fatalf(format, args...)
}

// decodeResponse unmarshals a raw JSON-RPC reply frame into a *jsonrpc.Response.
// A nil/empty frame (a notification, which yields no reply) decodes to a nil
// response so the assertions can report "no response".
func decodeResponse(raw []byte) (*jsonrpc.Response, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
