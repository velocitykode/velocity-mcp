package mcptest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/velocitykode/velocity"
	"github.com/velocitykode/velocity/events"
	"github.com/velocitykode/velocity/velocitytest"

	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
	_ "github.com/velocitykode/velocity-mcp/server/methods" // installs the full method set
	"github.com/velocitykode/velocity-mcp/transport"
)

// demoIntegrationServer builds an MCP server with one tool, one resource, and
// one prompt for the end-to-end integration test.
func demoIntegrationServer() *server.Server {
	return server.New("integration", "1.0.0",
		server.WithInstructions("integration server"),
		server.WithTools(
			server.NewTool("add", "Add two numbers").
				WithSchema(func(s *schema.Object) {
					s.Number("a").Required()
					s.Number("b").Required()
				}).
				HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
					return server.Text(formatNumber(req.Float("a") + req.Float("b"))), nil
				}),
		),
		server.WithResources(greetingResource{}),
		server.WithPrompts(echoPrompt{}),
	)
}

// postMCP drives one JSON-RPC message through the app's router over real HTTP and
// returns the decoded reply plus the response recorder for header assertions.
func postMCP(t *testing.T, app *velocity.App, sessionID, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	app.Router.ServeHTTP(rec, req)

	var decoded map[string]any
	if trimmed := strings.TrimSpace(rec.Body.String()); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			t.Fatalf("decode reply: %v (body=%q)", err, rec.Body.String())
		}
	}
	return rec, decoded
}

// buildIntegrationApp constructs a velocity test app, registers the MCP server in
// Services.Extensions["mcp"], wires its event dispatcher to fake (mirroring what
// velocity's bootstrap does for extensions registered before New), and mounts
// the HTTP transport on the router.
func buildIntegrationApp(t *testing.T, fake *events.FakeDispatcher) *velocity.App {
	t.Helper()
	app, err := velocitytest.NewApp(velocity.WithFakeEvents(fake))
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	srv := demoIntegrationServer()
	if err := server.RegisterServices(app.Services, srv); err != nil {
		t.Fatalf("register mcp server: %v", err)
	}

	// The server was registered after New() finished its wireInstanceEvents
	// sweep, so wire the dispatcher explicitly to the same fake the app uses.
	// FromServices retrieves the very server we registered, proving the
	// Extensions round-trip.
	resolved := server.FromServices(app.Services)
	if resolved == nil {
		t.Fatal("FromServices returned nil; server was not registered")
	}
	resolved.SetEventDispatcher(fake.Dispatch)

	app.Router.Post("/mcp", transport.Handler(resolved))
	return app
}

func TestIntegration_EndToEnd(t *testing.T) {
	fake := events.NewFakeDispatcher()
	app := buildIntegrationApp(t, fake)

	// 1. initialize over HTTP. The reply echoes the negotiated version and the
	//    transport surfaces the assigned session id in the response header.
	rec, init := postMCP(t, app, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli","version":"9"},"capabilities":{}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d", rec.Code)
	}
	sessionID := rec.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize did not assign a session id header")
	}
	result, _ := init["result"].(map[string]any)
	if result == nil || result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize result = %v", init["result"])
	}

	// 2. tools/list over HTTP, carrying the session id.
	_, list := postMCP(t, app, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if !containsToolNamed(tools, "add") {
		t.Fatalf("tools/list did not include add: %v", tools)
	}

	// 3. tools/call over HTTP.
	_, call := postMCP(t, app, sessionID,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":40}}}`)
	callResult := call["result"].(map[string]any)
	if callResult["isError"] != false {
		t.Fatalf("tools/call isError = %v", callResult["isError"])
	}
	first := callResult["content"].([]any)[0].(map[string]any)
	if first["text"] != "42" {
		t.Fatalf("tools/call text = %v, want 42", first["text"])
	}

	// 4. Events flowed through velocity's dispatcher: SessionInitialized on the
	//    initialize and ToolCalled on the call.
	if err := fake.AssertDispatched(event.SessionInitialized{}, nil); err != nil {
		t.Fatalf("SessionInitialized: %v", err)
	}
	if err := fake.AssertDispatched(event.ToolCalled{}, func(ev any) bool {
		tc, ok := ev.(event.ToolCalled)
		return ok && tc.Tool == "add"
	}); err != nil {
		t.Fatalf("ToolCalled: %v", err)
	}
}

func TestIntegration_NotificationReturns202(t *testing.T) {
	fake := events.NewFakeDispatcher()
	app := buildIntegrationApp(t, fake)

	rec, _ := postMCP(t, app, "",
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("notification body should be empty, got %q", rec.Body.String())
	}
}

func TestIntegration_ResourceAndPromptOverHTTP(t *testing.T) {
	fake := events.NewFakeDispatcher()
	app := buildIntegrationApp(t, fake)

	_, read := postMCP(t, app, "",
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file://greeting.txt"}}`)
	contents := read["result"].(map[string]any)["contents"].([]any)
	if len(contents) == 0 || contents[0].(map[string]any)["text"] != "hello world" {
		t.Fatalf("resources/read = %v", read["result"])
	}

	_, prompt := postMCP(t, app, "",
		`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"echo","arguments":{"topic":"go"}}}`)
	messages := prompt["result"].(map[string]any)["messages"].([]any)
	msgContent := messages[0].(map[string]any)["content"].(map[string]any)
	if msgContent["text"] != "topic is go" {
		t.Fatalf("prompts/get = %v", prompt["result"])
	}
}

func TestIntegration_ToolFailedEvent(t *testing.T) {
	fake := events.NewFakeDispatcher()
	app := buildIntegrationApp(t, fake)

	// An unknown tool is a protocol-level InvalidParams; the server reports it
	// via ToolFailed (never ToolCalled).
	postMCP(t, app, "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ghost","arguments":{}}}`)

	if err := fake.AssertDispatched(event.ToolFailed{}, nil); err != nil {
		t.Fatalf("ToolFailed: %v", err)
	}
	if err := fake.AssertNotDispatched(event.ToolCalled{}); err != nil {
		t.Fatalf("ToolCalled should not fire for an unknown tool: %v", err)
	}
}

// containsToolNamed reports whether a decoded tools list contains a tool with
// the given name.
func containsToolNamed(tools []any, name string) bool {
	for _, raw := range tools {
		if m, ok := raw.(map[string]any); ok {
			if m["name"] == name {
				return true
			}
		}
	}
	return false
}
