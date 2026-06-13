package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// TestMethodsRegistered_ViaTransportImport is a regression guard: the MCP
// protocol methods live in server/methods and self-register via init(), but a
// consumer that imports only server + transport must still get them. Importing
// transport (this package) blank-imports server/methods, so any serving path
// has tools/list, tools/call, and friends available without the consumer
// adding a magic blank import of their own.
//
// Before that import existed, initialize (hard-wired in server.handle) worked
// while every registry-backed method returned -32601, so this test drives a
// real server through Handle and asserts a non-initialize method resolves.
func TestMethodsRegistered_ViaTransportImport(t *testing.T) {
	srv := server.New("reg-test", "0.0.1",
		server.WithTools(
			server.NewTool("hello", "greet").
				WithSchema(func(s *schema.Object) { s.String("name") }).
				HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
					return server.Text("hi"), nil
				}),
		),
	)

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	res := srv.Handle(context.Background(), raw, "")
	if !res.HasResponse || res.Response == nil {
		t.Fatal("tools/list produced no response")
	}
	body, err := json.Marshal(res.Response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var envelope struct {
		Error  *struct{ Message string } `json:"error"`
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Error != nil {
		t.Fatalf("tools/list returned an error (methods not registered?): %s", envelope.Error.Message)
	}
	if envelope.Result == nil || len(envelope.Result.Tools) != 1 || envelope.Result.Tools[0].Name != "hello" {
		t.Fatalf("tools/list did not list the registered tool: %s", strings.TrimSpace(string(body)))
	}
}
