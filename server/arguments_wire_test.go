package server_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// greetPrompt echoes the argument it was invoked with, so a request that should
// have been refused cannot pass unnoticed as an empty call.
type greetPrompt struct{}

func (greetPrompt) Name() string                       { return "greet" }
func (greetPrompt) Description() string                { return "Greet someone" }
func (greetPrompt) Arguments() []server.PromptArgument { return nil }
func (greetPrompt) Handle(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("prompt:" + req.String("value")), nil
}

// docResource is the resource counterpart of greetPrompt.
type docResource struct{}

func (docResource) Name() string        { return "doc" }
func (docResource) Description() string { return "A document" }
func (docResource) URI() string         { return "file://doc.txt" }
func (docResource) MimeType() string    { return "text/plain" }
func (docResource) Read(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("resource:" + req.String("value")), nil
}

// argumentsServer registers one primitive of each kind that takes an argument
// bag, so the [arguments] rule can be exercised on every method that reads one.
func argumentsServer(t *testing.T) *server.Server {
	t.Helper()
	echo := server.NewTool("echo", "Echo the argument").
		WithSchema(func(s *schema.Object) { s.String("value") }).
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			return server.Text("tool:" + req.String("value")), nil
		})
	return server.New("demo", "1.0.0",
		server.WithTools(echo),
		server.WithPrompts(greetPrompt{}),
		server.WithResources(docResource{}),
	)
}

// TestArgumentsShapeOnTheWire asserts a request whose [arguments] member is not
// an object is refused with InvalidParams instead of being run with an empty
// bag. Running it would answer a call the client never made: a tool invoked
// with no arguments returns a plausible result for a request the server never
// understood.
func TestArgumentsShapeOnTheWire(t *testing.T) {
	const wantMessage = "Invalid params: The [arguments] member must be an object."
	bodies := map[string]func(string) string{
		"tools/call":     func(args string) string { return `"name":"echo","arguments":` + args },
		"prompts/get":    func(args string) string { return `"name":"greet","arguments":` + args },
		"resources/read": func(args string) string { return `"uri":"file://doc.txt","arguments":` + args },
	}
	arguments := []struct{ name, value string }{
		{"string", `"invalid"`},
		{"number", `7`},
		{"boolean", `true`},
		{"null", `null`},
		{"non-empty list", `[1,2]`},
		{"list of objects", `[{"value":"x"}]`},
	}

	for method, build := range bodies {
		for _, arg := range arguments {
			t.Run(method+"/"+arg.name, func(t *testing.T) {
				s := argumentsServer(t)
				res := handle(t, s, modernRequest(1, method, build(arg.value)))

				err := errorOf(t, res)
				if err.Code != jsonrpc.CodeInvalidParams {
					t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
				}
				if err.Message != wantMessage {
					t.Fatalf("message = %q, want %q", err.Message, wantMessage)
				}
				if got := string(res.Response.ID.Raw()); got != "1" {
					t.Fatalf("id = %s, want 1", got)
				}
			})
		}
	}
}

// TestArgumentsShapesThatAreAccepted asserts the rule refuses only what it must:
// an object is the canonical form, an empty list is the way some encoders render
// an empty map, and an absent member means the primitive takes no arguments.
func TestArgumentsShapesThatAreAccepted(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{"object", `"arguments":{"value":"hi"}`, "tool:hi"},
		{"empty object", `"arguments":{}`, "tool:"},
		{"empty list", `"arguments":[]`, "tool:"},
		{"absent", ``, "tool:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := argumentsServer(t)
			extra := `"name":"echo"`
			if tt.args != "" {
				extra += "," + tt.args
			}
			res := handle(t, s, modernRequest(1, "tools/call", extra))

			result := decodeResult(t, res.Response)
			if got := toolText(t, result); got != tt.want {
				t.Fatalf("tool ran with %q, want %q", got, tt.want)
			}
		})
	}
}

// TestArgumentsShapeOnParamsThatCarryNoMembers asserts the rule looks past the
// one params form that carries no members at all: an empty list stands in for an
// empty object, so there is no [arguments] member to judge and the handler
// reports the parameter it actually needs.
func TestArgumentsShapeOnParamsThatCarryNoMembers(t *testing.T) {
	s := argumentsServer(t)
	res := handle(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":[]}`)

	err := errorOf(t, res)
	if err.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
	}
	if err.Message != "Missing [name] parameter." {
		t.Fatalf("message = %q, want the handler's own complaint", err.Message)
	}
}

// TestArgumentsShapeIsCheckedForLegacyRequestsToo asserts the rule is about the
// message, not about the handshake it was made under: a client that predates the
// discovery handshake gets the same refusal.
func TestArgumentsShapeIsCheckedForLegacyRequestsToo(t *testing.T) {
	s := argumentsServer(t)
	res := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":"invalid"}}`)

	err := errorOf(t, res)
	if err.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
	}
	if err.Message != "Invalid params: The [arguments] member must be an object." {
		t.Fatalf("message = %q", err.Message)
	}
}

// TestUnknownMethodOutranksTheArgumentsShape asserts a method the server does
// not implement is reported as such even when the request also carries a
// malformed argument bag: the shape of a bag no handler will read is not what
// the client has to fix first.
func TestUnknownMethodOutranksTheArgumentsShape(t *testing.T) {
	s := argumentsServer(t)
	res := handle(t, s, modernRequest(1, "does/not/exist", `"arguments":"invalid"`))

	err := errorOf(t, res)
	if err.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeMethodNotFound)
	}
}

// echoArgumentsMethod is an extension method whose parameter schema models
// [arguments] as whatever the caller sends. It echoes the member back verbatim,
// so a request the server refused before dispatch is distinguishable from one
// the handler actually read.
type echoArgumentsMethod struct{}

func (echoArgumentsMethod) Handle(_ *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	var params struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Invalid params: The [params] member must be an object.")
	}
	return jsonrpc.NewResult(req.ID, map[string]any{"echoed": params.Arguments})
}

// TestCustomMethodOwnsItsArgumentsMember asserts the object-bag rule applies to
// the methods the protocol gives an [arguments] member and to no others. A
// method registered through WithMethod defines its own parameters, so a list or
// a scalar under that name is its handler's to read; refusing it before dispatch
// would make the name reserved across the whole surface.
func TestCustomMethodOwnsItsArgumentsMember(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{"list of numbers", `[1,2]`, `[1,2]`},
		{"string", `"invalid"`, `"invalid"`},
		{"number", `7`, `7`},
		{"boolean", `true`, `true`},
		{"null", `null`, `null`},
		{"object still reaches the handler", `{"value":"x"}`, `{"value":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", server.WithMethod("acme/report", echoArgumentsMethod{}))
			res := handle(t, s, modernRequest(1, "acme/report", `"arguments":`+tt.args))

			if res.Response.Error != nil {
				t.Fatalf("the extension method never ran: %+v", res.Response.Error)
			}
			var result struct {
				Echoed json.RawMessage `json:"echoed"`
			}
			if err := json.Unmarshal(res.Response.Result, &result); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if string(result.Echoed) != tt.want {
				t.Fatalf("handler read arguments %s, want %s", result.Echoed, tt.want)
			}
		})
	}
}

// TestCustomMethodReplacingAPrimitiveKeepsTheRule asserts the rule follows the
// method name the specification defines, not the handler behind it: a server
// that replaces tools/call still answers a malformed argument bag the way the
// protocol says it must.
func TestCustomMethodReplacingAPrimitiveKeepsTheRule(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("tools/call", echoArgumentsMethod{}))
	res := handle(t, s, modernRequest(1, "tools/call", `"name":"echo","arguments":[1,2]`))

	err := errorOf(t, res)
	if err.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
	}
	if err.Message != "Invalid params: The [arguments] member must be an object." {
		t.Fatalf("message = %q", err.Message)
	}
}

// toolText pulls the single text block out of a tools/call result.
func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("result content = %v, want a single block", result["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content block = %v", content[0])
	}
	text, ok := block["text"].(string)
	if !ok {
		t.Fatalf("content block has no text: %v", block)
	}
	return text
}
