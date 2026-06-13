package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

func newTestClient(f *fakeTransport) *Client {
	return New(f, schema.NewImplementation("test", "0.0.1"))
}

func TestConnectInitialize(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !c.Connected() {
		t.Fatal("expected connected")
	}
	res := c.InitializeResult()
	if res == nil || res.ServerInfo.Name != "fake" || res.Instructions != "be helpful" {
		t.Fatalf("initialize result = %+v", res)
	}
	// The handshake sends initialize then the initialized notification.
	if len(f.sent) != 2 {
		t.Fatalf("expected 2 frames sent during connect, got %d", len(f.sent))
	}
}

func TestConnectRejectsBadProtocolVersion(t *testing.T) {
	f := newFakeTransport()
	f.on("initialize", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"protocolVersion": "1999-01-01",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake", "version": "1.0.0"},
		})
		return resp
	})
	c := newTestClient(f)
	if err := c.Connect(context.Background()); err == nil {
		t.Fatal("expected error for unsupported protocol version")
	}
}

func TestPing(t *testing.T) {
	f := newFakeTransport()
	c := newTestClient(f)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestCallTool(t *testing.T) {
	f := newFakeTransport()
	f.on("tools/call", func(id jsonrpc.ID, params json.RawMessage) *jsonrpc.Response {
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Name != "echo" || p.Arguments["msg"] != "hi" {
			return jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeInvalidParams, "bad params")
		}
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "hi"}},
			"isError": false,
		})
		return resp
	})
	c := newTestClient(f)

	res, err := c.CallTool(context.Background(), "echo", map[string]any{"msg": "hi"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError || res.Text() != "hi" {
		t.Fatalf("result = %+v text=%q", res, res.Text())
	}
}

func TestCallToolServerError(t *testing.T) {
	f := newFakeTransport()
	f.on("tools/call", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		return jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeInvalidParams, "nope")
	})
	c := newTestClient(f)

	_, err := c.CallTool(context.Background(), "x", nil)
	var rpcErr *jsonrpc.Error
	if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected jsonrpc error, got %v", err)
	}
}

func TestToolsPaginationAndBinding(t *testing.T) {
	f := newFakeTransport()
	f.on("tools/list", func(id jsonrpc.ID, params json.RawMessage) *jsonrpc.Response {
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Cursor == "" {
			resp, _ := jsonrpc.NewResult(id, map[string]any{
				"tools":      []any{map[string]any{"name": "a"}},
				"nextCursor": "page2",
			})
			return resp
		}
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"tools": []any{map[string]any{"name": "b"}},
		})
		return resp
	})
	f.on("tools/call", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{"content": []any{}, "isError": false})
		return resp
	})
	c := newTestClient(f)

	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Fatalf("tools = %+v", tools)
	}
	// A listed tool is bound to its client and is callable.
	if _, err := tools[0].Call(context.Background(), nil); err != nil {
		t.Fatalf("bound call: %v", err)
	}
	// An unbound tool reports a clear error.
	if _, err := (Tool{Name: "loose"}).Call(context.Background(), nil); err == nil {
		t.Fatal("expected unbound tool error")
	}
}

func TestToolsLimit(t *testing.T) {
	f := newFakeTransport()
	f.on("tools/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"tools":      []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
			"nextCursor": "more",
		})
		return resp
	})
	c := newTestClient(f)

	tools, err := c.Tools(context.Background(), 1)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool with limit, got %d", len(tools))
	}

	if _, err := c.Tools(context.Background(), -1); err == nil {
		t.Fatal("expected error for negative limit")
	}
	if got, _ := c.Tools(context.Background(), 0); len(got) != 0 {
		t.Fatalf("limit 0 should return no tools, got %d", len(got))
	}
}

func TestPaginationCursorLoopGuard(t *testing.T) {
	f := newFakeTransport()
	f.on("resources/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		// Always returns the same cursor: the client must break the loop.
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"resources":  []any{map[string]any{"uri": "file://x", "name": "x"}},
			"nextCursor": "stuck",
		})
		return resp
	})
	c := newTestClient(f)
	if _, err := c.Resources(context.Background()); err == nil {
		t.Fatal("expected repeated-cursor error")
	}
}

func TestReadResourceAndGetPrompt(t *testing.T) {
	f := newFakeTransport()
	f.on("resources/read", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"contents": []any{map[string]any{"uri": "file://x", "mimeType": "text/plain", "text": "body"}},
		})
		return resp
	})
	f.on("prompts/get", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"description": "d",
			"messages": []any{map[string]any{
				"role":    "user",
				"content": map[string]any{"type": "text", "text": "hello"},
			}},
		})
		return resp
	})
	c := newTestClient(f)

	rr, err := c.ReadResource(context.Background(), "file://x")
	if err != nil || rr.MimeType() != "text/plain" || rr.Content() != "body" {
		t.Fatalf("read resource = %+v err=%v", rr, err)
	}
	pr, err := c.GetPrompt(context.Background(), "p", nil)
	if err != nil || pr.Description != "d" || pr.Text() != "hello" {
		t.Fatalf("get prompt = %+v err=%v", pr, err)
	}
}

func TestSessionExpiryReconnects(t *testing.T) {
	f := newFakeTransport()
	f.expireOnce["tools/call"] = true
	f.on("tools/call", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{"content": []any{}, "isError": false})
		return resp
	})
	c := newTestClient(f)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	connectsBefore := f.connects

	if _, err := c.CallTool(context.Background(), "x", nil); err != nil {
		t.Fatalf("call after expiry: %v", err)
	}
	if f.connects != connectsBefore+1 {
		t.Fatalf("expected one reconnect, connects went %d -> %d", connectsBefore, f.connects)
	}
}

func TestServerPingDuringDispatch(t *testing.T) {
	f := newFakeTransport()
	// Inject a server-initiated ping before the tools/call response. The client
	// must answer it and still return the real response.
	serverPing, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 999, "method": "ping"})
	f.framesBefore["tools/call"] = []string{string(serverPing)}
	f.on("tools/call", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{"content": []any{}, "isError": false})
		return resp
	})
	c := newTestClient(f)

	if _, err := c.CallTool(context.Background(), "x", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	// The client must have sent a response to the server's ping (id 999).
	answered := false
	for _, s := range f.sent {
		if strings.Contains(s, `"id":999`) && strings.Contains(s, `"result"`) {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("client did not answer server ping; sent=%v", f.sent)
	}
}

func TestRecipeRoundTrip(t *testing.T) {
	r := Recipe{Driver: "http", URL: "https://example.com/mcp", Token: "abc"}
	tr, err := TransportFromRecipe(r)
	if err != nil {
		t.Fatalf("from recipe: %v", err)
	}
	if got := tr.Recipe(); got.Driver != "http" || got.URL != r.URL || got.Token != "abc" {
		t.Fatalf("recipe round-trip = %+v", got)
	}
	if _, err := TransportFromRecipe(Recipe{Driver: "nope"}); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}
