package server_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// serverInfoOf pulls the server implementation metadata out of a result's
// _meta, failing the test when it is absent.
func serverInfoOf(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	meta, ok := result["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("result carries no _meta: %v", result)
	}
	info, ok := meta[server.MetaKeyServerInfo].(map[string]any)
	if !ok {
		t.Fatalf("_meta carries no server info: %v", meta)
	}
	return info
}

// TestResultEnvelopeOnEveryMethod asserts every successful result carries the
// envelope, whichever method produced it and whichever handshake the client
// used. A host reads the server identity off any result, so no method may be
// the exception.
func TestResultEnvelopeOnEveryMethod(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"legacy initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`},
		{"legacy ping", `{"jsonrpc":"2.0","id":1,"method":"ping"}`},
		{"legacy tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"modern discover", modernRequest(1, "server/discover")},
		{"modern tools/list", modernRequest(1, "tools/list")},
		{"modern tools/call", modernRequest(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)},
		{"modern resources/list", modernRequest(1, "resources/list")},
		{"modern prompts/list", modernRequest(1, "prompts/list")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "9.9", server.WithTools(addTool()))
			result := decodeResult(t, handle(t, s, tt.raw).Response)
			if result["resultType"] != "complete" {
				t.Fatalf("resultType = %v, want complete", result["resultType"])
			}
			info := serverInfoOf(t, result)
			if info["name"] != "demo" || info["version"] != "9.9" {
				t.Fatalf("server info = %v", info)
			}
		})
	}
}

// TestResultEnvelopeCarriesTheFullImplementation asserts the envelope reports
// the same implementation metadata the handshake advertises, optional fields
// included, rather than a trimmed name/version pair.
func TestResultEnvelopeCarriesTheFullImplementation(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithTitle("Demo Server"),
		server.WithWebsiteURL("https://example.test"),
	)
	info := serverInfoOf(t, decodeResult(t, handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`).Response))
	if info["title"] != "Demo Server" || info["websiteUrl"] != "https://example.test" {
		t.Fatalf("server info = %v", info)
	}
}

// TestResultEnvelopeSkipsErrors asserts an error response is delivered exactly
// as built: an error is not a result, so it carries neither a result type nor
// the server metadata.
func TestResultEnvelopeSkipsErrors(t *testing.T) {
	tests := []struct{ name, raw string }{
		{"unknown method", modernRequest(1, "no/such/method")},
		{"unsupported version", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}}`},
		{"malformed json", `{"jsonrpc":"2.0",`},
		{"unresolvable resource", modernRequest(1, "resources/read", `"uri":"file://nope"`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, tt.raw)
			if res.Response.Error == nil {
				t.Fatalf("expected an error response, got %s", res.Response.Result)
			}
			if len(res.Response.Result) != 0 {
				t.Fatalf("an error response must carry no result, got %s", res.Response.Result)
			}
		})
	}
}

// TestResultEnvelopePreservesHandlerMeta asserts a handler's own _meta keys
// survive: the envelope adds the server info beside them instead of replacing
// the bag.
func TestResultEnvelopePreservesHandlerMeta(t *testing.T) {
	tool := server.NewTool("traced", "carries its own metadata").
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return server.NewResponse(content.NewText("ok")).WithMeta("app/trace", "abc"), nil
		})
	s := server.New("demo", "1.0.0", server.WithTools(tool))

	result := decodeResult(t, handle(t, s, modernRequest(1, "tools/call", `"name":"traced"`)).Response)
	meta := result["_meta"].(map[string]any)
	if meta["app/trace"] != "abc" {
		t.Fatalf("handler metadata lost: %v", meta)
	}
	if _, ok := meta[server.MetaKeyServerInfo]; !ok {
		t.Fatalf("server info missing beside handler metadata: %v", meta)
	}
}

// opinionatedMethod is a custom handler that states its own result type and
// metadata, as a method that suspends a call awaiting client input would.
type opinionatedMethod struct{}

func (opinionatedMethod) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, map[string]any{
		"resultType": "input_required",
		"_meta":      map[string]any{"app/trace": "abc"},
	})
}

// TestResultEnvelopeKeepsHandlerResultType asserts a handler that declares its
// own result type keeps it. The envelope supplies a default, not an override:
// overwriting it would turn a suspended call into a finished one.
func TestResultEnvelopeKeepsHandlerResultType(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("custom/method", opinionatedMethod{}))

	result := decodeResult(t, handle(t, s, modernRequest(1, "custom/method")).Response)
	if result["resultType"] != "input_required" {
		t.Fatalf("resultType = %v, want input_required", result["resultType"])
	}
	meta := result["_meta"].(map[string]any)
	if meta["app/trace"] != "abc" {
		t.Fatalf("handler metadata lost: %v", meta)
	}
	if _, ok := meta[server.MetaKeyServerInfo]; !ok {
		t.Fatalf("server info missing: %v", meta)
	}
}

// nonObjectMethod returns a result that is not a JSON object, which the
// protocol does not use but a custom handler could produce.
type nonObjectMethod struct{}

func (nonObjectMethod) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, []int{1, 2, 3})
}

// TestResultEnvelopeLeavesNonObjectResults asserts a result the envelope cannot
// decorate is passed through untouched rather than reshaped or dropped.
func TestResultEnvelopeLeavesNonObjectResults(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("custom/list", nonObjectMethod{}))

	res := handle(t, s, modernRequest(1, "custom/list"))
	if res.Response.Error != nil {
		t.Fatalf("unexpected error: %+v", res.Response.Error)
	}
	if got := string(res.Response.Result); got != `[1,2,3]` {
		t.Fatalf("result = %s, want [1,2,3]", got)
	}
}

// TestWithMethodOverridesBuiltIn asserts a custom handler replaces the built-in
// of the same name.
func TestWithMethodOverridesBuiltIn(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("tools/list", opinionatedMethod{}))
	result := decodeResult(t, handle(t, s, modernRequest(1, "tools/list")).Response)
	if result["resultType"] != "input_required" {
		t.Fatalf("built-in tools/list was not replaced: %v", result)
	}
}

// TestWithMethodIgnoresIncompleteRegistrations asserts a nil handler or an empty
// name is dropped rather than installed, so a misconfigured option cannot make
// the server answer a method with nothing.
func TestWithMethodIgnoresIncompleteRegistrations(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithMethod("", opinionatedMethod{}),
		server.WithMethod("custom/method", nil),
	)
	if code := errorOf(t, handle(t, s, modernRequest(1, "custom/method"))).Code; code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("code = %d, want method not found", code)
	}
}

// TestNotificationsAreNotEnveloped asserts a server-initiated frame stays a
// notification: the envelope applies to results, and a notification has none.
func TestNotificationsAreNotEnveloped(t *testing.T) {
	s := server.New("demo", "1.0.0")
	var frames [][]byte
	emit := func(msg []byte) error {
		frames = append(frames, append([]byte(nil), msg...))
		return nil
	}
	s.HandleStream(context.Background(), []byte(modernRequest(5, "subscriptions/listen")), "", emit)

	if len(frames) != 1 {
		t.Fatalf("expected one emitted frame, got %d", len(frames))
	}
	var frame map[string]any
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if _, ok := frame["result"]; ok {
		t.Fatalf("a notification carries no result: %s", frames[0])
	}
	params := frame["params"].(map[string]any)
	if _, ok := params["resultType"]; ok {
		t.Fatalf("a notification must not be enveloped: %s", frames[0])
	}
}
