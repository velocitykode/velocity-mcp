package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

func TestResourcesAndPrompts(t *testing.T) {
	f := newFakeTransport()
	f.on("resources/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"resources": []any{map[string]any{
				"uri": "file://a", "name": "a", "title": "A", "mimeType": "text/plain", "size": float64(12),
			}},
		})
		return resp
	})
	f.on("prompts/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"prompts": []any{map[string]any{
				"name": "p", "description": "d",
				"arguments": []any{map[string]any{"name": "topic", "required": true}},
			}},
		})
		return resp
	})
	c := newTestClient(f)

	resources, err := c.Resources(context.Background())
	if err != nil || len(resources) != 1 {
		t.Fatalf("resources = %+v err=%v", resources, err)
	}
	if resources[0].MimeType != "text/plain" || resources[0].Size == nil || *resources[0].Size != 12 {
		t.Fatalf("resource = %+v", resources[0])
	}

	prompts, err := c.Prompts(context.Background())
	if err != nil || len(prompts) != 1 {
		t.Fatalf("prompts = %+v err=%v", prompts, err)
	}
	if prompts[0].Name != "p" || len(prompts[0].Arguments) != 1 {
		t.Fatalf("prompt = %+v", prompts[0])
	}
}

func TestLocalConstructsStdio(t *testing.T) {
	c := Local("cat", "--flag")
	if c == nil || c.transport.Recipe().Driver != "stdio" {
		t.Fatalf("Local should build a stdio client: %+v", c)
	}
	if c.transport.Recipe().Command != "cat" {
		t.Fatalf("recipe command = %q", c.transport.Recipe().Command)
	}
}

func TestErrorWrapping(t *testing.T) {
	cause := errors.New("root cause")
	err := wrapError(cause, "outer")
	if err.Error() != "outer: root cause" {
		t.Fatalf("error string = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped error should unwrap to cause")
	}
	if newError("plain").Error() != "plain" {
		t.Fatal("plain error string")
	}
}

func TestHTTPTransportSetTimeoutAndRecipe(t *testing.T) {
	tr := NewHTTPTransport("https://example.com/mcp")
	tr.SetTimeout(2 * time.Second)
	tr.WithToken("abc")
	r := tr.Recipe()
	if r.Driver != "http" || r.Timeout != 2*time.Second || r.Token != "abc" {
		t.Fatalf("recipe = %+v", r)
	}
}

func TestTransportFromRecipeStdio(t *testing.T) {
	tr, err := TransportFromRecipe(Recipe{Driver: "stdio", Command: "cat", Args: []string{"-u"}, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("from recipe: %v", err)
	}
	r := tr.Recipe()
	if r.Command != "cat" || r.Timeout != 3*time.Second || len(r.Args) != 1 {
		t.Fatalf("recipe = %+v", r)
	}
	if _, err := TransportFromRecipe(Recipe{Driver: "stdio"}); err == nil {
		t.Fatal("expected error for stdio recipe without command")
	}
	if _, err := TransportFromRecipe(Recipe{Driver: "http"}); err == nil {
		t.Fatal("expected error for http recipe without url")
	}
}

func TestStdioClosedOutput(t *testing.T) {
	// `true` exits immediately without writing: Receive must report a clear
	// closed-output error rather than hanging.
	tr := NewStdioTransport("true")
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()
	if _, err := tr.Receive(context.Background()); err == nil {
		t.Fatal("expected closed-output error")
	}
}

func TestWebClientWithTokenFunc(t *testing.T) {
	var auth string
	srv := mcpHTTPServer(t, func(a string) {
		if a != "" {
			auth = a
		}
	})
	w := Web(srv.URL).WithTokenFunc(func() string { return "dyn-token" })
	if err := w.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if auth != "Bearer dyn-token" {
		t.Fatalf("authorization = %q", auth)
	}
}
