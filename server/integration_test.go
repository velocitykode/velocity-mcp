package server_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
	_ "github.com/velocitykode/velocity-mcp/server/methods" // installs the full method set
)

// addTool is a closure tool exercised by the integration tests.
func addTool() server.Tool {
	return server.NewTool("add", "Add two numbers").
		WithSchema(func(s *schema.Object) {
			s.Number("a").Required()
			s.Number("b").Required()
		}).
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return server.Text(formatNumber(req.Float("a") + req.Float("b"))), nil
		})
}

func formatNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// recordingDispatcher captures dispatched events for assertions. It is a local
// fake (rather than events.FakeDispatcher) so the test stays decoupled from the
// framework event package and is safe under -race.
type recordingDispatcher struct {
	mu     sync.Mutex
	events []any
}

func (d *recordingDispatcher) fn() func(context.Context, any) error {
	return func(_ context.Context, ev any) error {
		d.mu.Lock()
		d.events = append(d.events, ev)
		d.mu.Unlock()
		return nil
	}
}

func (d *recordingDispatcher) all() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]any(nil), d.events...)
}

func decodeResult(t *testing.T, resp *jsonrpc.Response) map[string]any {
	t.Helper()
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Result, &m); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return m
}

func handle(t *testing.T, s *server.Server, raw string) server.HandleResult {
	t.Helper()
	return s.Handle(context.Background(), []byte(raw), "sess-1")
}

func TestInitializeGolden(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithInstructions("hello instructions"))
	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli","version":"9"}}}`)

	result := decodeResult(t, res.Response)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	if result["instructions"] != "hello instructions" {
		t.Fatalf("instructions = %v", result["instructions"])
	}
	serverInfo := result["serverInfo"].(map[string]any)
	if serverInfo["name"] != "demo" || serverInfo["version"] != "1.0.0" {
		t.Fatalf("serverInfo = %v", serverInfo)
	}
	caps := result["capabilities"].(map[string]any)
	for _, c := range []string{"tools", "resources", "prompts"} {
		if _, ok := caps[c]; !ok {
			t.Fatalf("missing capability %q", c)
		}
	}
	if res.SessionID == "" {
		t.Fatal("initialize should assign a session id")
	}
}

func TestInitializeDispatchesSessionInitialized(t *testing.T) {
	s := server.New("demo", "1.0.0")
	d := &recordingDispatcher{}
	s.SetEventDispatcher(d.fn())

	handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli","version":"9"},"capabilities":{"roots":{}}}}`)

	events := d.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev, ok := events[0].(interface {
		Name() string
		ClientName() string
	})
	if !ok {
		t.Fatalf("unexpected event type %T", events[0])
	}
	if ev.Name() != "mcp.session.initialized" {
		t.Fatalf("event name = %q", ev.Name())
	}
	if ev.ClientName() != "cli" {
		t.Fatalf("client name = %q", ev.ClientName())
	}
}

func TestCallToolGolden(t *testing.T) {
	s := server.New("calc", "1.0.0", server.WithTools(addTool()))
	res := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3}}}`)

	result := decodeResult(t, res.Response)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	contentArr := result["content"].([]any)
	if len(contentArr) != 1 {
		t.Fatalf("content len = %d", len(contentArr))
	}
	item := contentArr[0].(map[string]any)
	if item["type"] != "text" || item["text"] != "5" {
		t.Fatalf("content item = %v", item)
	}
}

func TestCallToolDispatchesEvents(t *testing.T) {
	s := server.New("calc", "1.0.0", server.WithTools(addTool()))
	d := &recordingDispatcher{}
	s.SetEventDispatcher(d.fn())

	handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"a":2,"b":3}}}`)

	events := d.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	named := events[0].(interface{ Name() string })
	if named.Name() != "mcp.tool.called" {
		t.Fatalf("event = %q", named.Name())
	}
}

func TestCallToolUnknownIsInvalidParams(t *testing.T) {
	s := server.New("calc", "1.0.0", server.WithTools(addTool()))
	res := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"missing","arguments":{}}}`)
	if res.Response.Error != nil {
		// laravel returns this as a protocol error response; our handler maps
		// it onto an error response too. Either way the code is InvalidParams.
		if res.Response.Error.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("error code = %d", res.Response.Error.Code)
		}
		return
	}
	t.Fatal("expected an error for an unknown tool")
}

func TestCallToolMissingNameDispatchesNoToolCalled(t *testing.T) {
	s := server.New("calc", "1.0.0", server.WithTools(addTool()))
	d := &recordingDispatcher{}
	s.SetEventDispatcher(d.fn())
	res := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"arguments":{}}}`)
	if res.Response.Error == nil || res.Response.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected invalid params, got %+v", res.Response.Error)
	}
	// A missing-name failure is reported via ToolFailed, not ToolCalled.
	for _, ev := range d.all() {
		if n, ok := ev.(interface{ Name() string }); ok && n.Name() == "mcp.tool.called" {
			t.Fatal("ToolCalled should not fire for a missing-name error")
		}
	}
}

func TestPing(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	result := decodeResult(t, res.Response)
	if len(result) != 0 {
		t.Fatalf("ping result should be empty, got %v", result)
	}
}

// TestToolReceivesRequestContext asserts that the inbound request context is
// threaded all the way to a tool handler (rather than the handler always seeing
// a fresh context.Background()), so a tool can observe client cancellation and
// request deadlines end-to-end.
func TestToolReceivesRequestContext(t *testing.T) {
	type ctxKey string
	const key ctxKey = "trace"

	var (
		mu        sync.Mutex
		gotValue  any
		gotCancel bool
	)
	capturing := server.NewTool("capture", "captures its context").
		HandleFunc(func(ctx context.Context, _ *server.Request) (*server.Response, error) {
			mu.Lock()
			gotValue = ctx.Value(key)
			gotCancel = ctx.Err() != nil
			mu.Unlock()
			return server.Text("ok"), nil
		})

	s := server.New("demo", "1.0.0", server.WithTools(capturing))

	// A context carrying a value and already cancelled: both must be visible to
	// the tool handler.
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key, "abc"))
	cancel()

	res := s.Handle(ctx, []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"capture","arguments":{}}}`), "sess-1")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("unexpected error response: %+v", res.Response)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotValue != "abc" {
		t.Fatalf("tool did not observe request-context value: got %v", gotValue)
	}
	if !gotCancel {
		t.Fatal("tool did not observe request-context cancellation")
	}
}

// TestNullIDRoutedAsNotification asserts a present-but-null id is routed as a
// notification (no reply), matching laravel/mcp's isset()-based routing.
func TestNullIDRoutedAsNotification(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`), "sess-1")
	if res.HasResponse {
		t.Fatalf("present-but-null id should produce no reply, got %+v", res.Response)
	}
}

func TestMethodNotFound(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, `{"jsonrpc":"2.0","id":3,"method":"does/not/exist"}`)
	if res.Response.Error == nil || res.Response.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("expected method not found, got %+v", res.Response.Error)
	}
}
