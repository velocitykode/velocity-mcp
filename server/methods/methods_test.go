package methods

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity/validation"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// --- test fixtures ---

func echoTool() server.Tool {
	return server.NewTool("echo", "echoes the message").
		WithSchema(func(s *schema.Object) { s.String("msg").Required() }).
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return server.Text(req.String("msg")), nil
		})
}

func validatingTool() server.Tool {
	return server.NewTool("strict", "validates input").
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			if err := req.Validate(validation.Rules{"name": {"required"}}); err != nil {
				return nil, err
			}
			return server.Text("ok"), nil
		})
}

func failingTool() server.Tool {
	return server.NewTool("boom", "always fails").
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return nil, context.DeadlineExceeded
		})
}

type docResource struct{}

func (docResource) Name() string        { return "doc" }
func (docResource) Description() string { return "a document" }
func (docResource) URI() string         { return "file://doc.txt" }
func (docResource) MimeType() string    { return "text/plain" }
func (docResource) Read(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("contents of " + req.URI()), nil
}

type userTemplate struct{}

func (userTemplate) Name() string        { return "user" }
func (userTemplate) Description() string { return "a user" }
func (userTemplate) URI() string         { return "file://users/{id}" }
func (userTemplate) URITemplate() string { return "file://users/{id}" }
func (userTemplate) MimeType() string    { return "application/json" }
func (userTemplate) Read(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text(req.String("id")), nil
}

type greetPrompt struct{}

func (greetPrompt) Name() string        { return "greet" }
func (greetPrompt) Description() string { return "greets a person" }
func (greetPrompt) Arguments() []server.PromptArgument {
	return []server.PromptArgument{server.NewPromptArgument("name", "who", true)}
}
func (greetPrompt) Handle(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("hello " + req.String("name")).AsAssistant(), nil
}

func req(t *testing.T, id int, method string, params map[string]any) *jsonrpc.Request {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = b
	}
	return &jsonrpc.Request{JSONRPC: jsonrpc.Version, ID: jsonrpc.IntID(int64(id)), Method: method, Params: raw}
}

func ctxWith(opts ...server.Option) *server.Context {
	return server.New("test", "1.0.0", opts...).NewTestContext()
}

func decodeResult(t *testing.T, resp *jsonrpc.Response) map[string]any {
	t.Helper()
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Result, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// --- tests ---

func TestPingHandler(t *testing.T) {
	resp, err := Ping{}.Handle(ctxWith(), req(t, 1, "ping", nil))
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	m := decodeResult(t, resp)
	if len(m) != 0 {
		t.Fatalf("ping result = %v", m)
	}
}

func TestListTools(t *testing.T) {
	c := ctxWith(server.WithTools(echoTool()))
	resp, err := ListTools{}.Handle(c, req(t, 1, "tools/list", nil))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	m := decodeResult(t, resp)
	tools := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" {
		t.Fatalf("tool = %v", tool)
	}
	input := tool["inputSchema"].(map[string]any)
	if _, ok := input["properties"]; !ok {
		t.Fatal("inputSchema missing properties")
	}
}

func TestListToolsPagination(t *testing.T) {
	c := ctxWith(
		server.WithTools(echoTool(), failingTool(), validatingTool()),
		server.WithPageSize(2),
	)
	resp, _ := ListTools{}.Handle(c, req(t, 1, "tools/list", nil))
	m := decodeResult(t, resp)
	if len(m["tools"].([]any)) != 2 {
		t.Fatalf("first page = %v", m["tools"])
	}
	next, ok := m["nextCursor"].(string)
	if !ok || next == "" {
		t.Fatal("expected a next cursor")
	}

	resp2, _ := ListTools{}.Handle(c, req(t, 2, "tools/list", map[string]any{"cursor": next}))
	m2 := decodeResult(t, resp2)
	if len(m2["tools"].([]any)) != 1 {
		t.Fatalf("second page = %v", m2["tools"])
	}
	if _, ok := m2["nextCursor"]; ok {
		t.Fatal("last page should not have a next cursor")
	}
}

// TestListTools_PerPageZeroEmptyPage asserts an explicit per_page of 0 yields an
// empty page with no nextCursor (min(0, max) = 0), while
// an absent per_page falls back to the default page size.
func TestListTools_PerPageZeroEmptyPage(t *testing.T) {
	c := ctxWith(server.WithTools(echoTool(), failingTool(), validatingTool()))

	// Explicit zero -> empty page, no next cursor.
	resp, _ := ListTools{}.Handle(c, req(t, 1, "tools/list", map[string]any{"per_page": 0}))
	m := decodeResult(t, resp)
	if len(m["tools"].([]any)) != 0 {
		t.Fatalf("per_page:0 should yield an empty page, got %v", m["tools"])
	}
	if _, ok := m["nextCursor"]; ok {
		t.Fatal("per_page:0 should not produce a next cursor")
	}

	// Absent per_page -> default page size returns all three tools (default is
	// well above 3), with no next cursor.
	resp2, _ := ListTools{}.Handle(c, req(t, 2, "tools/list", nil))
	m2 := decodeResult(t, resp2)
	if len(m2["tools"].([]any)) != 3 {
		t.Fatalf("absent per_page should use the default size, got %v", m2["tools"])
	}
}

func TestCallToolSuccess(t *testing.T) {
	c := ctxWith(server.WithTools(echoTool()))
	resp, err := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"msg": "hi"},
	}))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	m := decodeResult(t, resp)
	if m["isError"] != false {
		t.Fatalf("isError = %v", m["isError"])
	}
	item := m["content"].([]any)[0].(map[string]any)
	if item["text"] != "hi" {
		t.Fatalf("text = %v", item["text"])
	}
}

func TestCallToolMissingName(t *testing.T) {
	c := ctxWith(server.WithTools(echoTool()))
	resp, _ := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{"arguments": map[string]any{}}))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected invalid params, got %+v", resp.Error)
	}
}

func TestCallToolNotFound(t *testing.T) {
	c := ctxWith(server.WithTools(echoTool()))
	resp, _ := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{"name": "missing"}))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected not found, got %+v", resp.Error)
	}
}

func TestCallToolValidationBecomesErrorResult(t *testing.T) {
	c := ctxWith(server.WithTools(validatingTool()))
	resp, err := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{
		"name":      "strict",
		"arguments": map[string]any{},
	}))
	if err != nil {
		t.Fatalf("validation should not be a Go error: %v", err)
	}
	m := decodeResult(t, resp)
	if m["isError"] != true {
		t.Fatalf("expected isError result, got %v", m)
	}
}

func TestCallToolUnexpectedErrorPropagates(t *testing.T) {
	c := ctxWith(server.WithTools(failingTool()))
	_, err := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{"name": "boom"}))
	if err == nil {
		t.Fatal("an unexpected handler error should propagate to the server")
	}
}

func TestCallToolBlobContentBecomesErrorResult(t *testing.T) {
	// A Blob cannot be represented in a tool result; the handler degrades to an
	// error result rather than failing the call.
	tool := server.NewTool("blobby", "returns a blob").
		HandleFunc(func(ctx context.Context, r *server.Request) (*server.Response, error) {
			return server.NewResponse(content.NewBlob([]byte("data"))), nil
		})
	c := ctxWith(server.WithTools(tool))
	resp, err := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{"name": "blobby"}))
	if err != nil {
		t.Fatalf("should not error: %v", err)
	}
	m := decodeResult(t, resp)
	if m["isError"] != true {
		t.Fatalf("expected error result for unrepresentable content, got %v", m)
	}
}

func TestReadResourceExact(t *testing.T) {
	c := ctxWith(server.WithResources(docResource{}))
	resp, err := ReadResource{}.Handle(c, req(t, 1, "resources/read", map[string]any{"uri": "file://doc.txt"}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m := decodeResult(t, resp)
	contents := m["contents"].([]any)
	item := contents[0].(map[string]any)
	if item["uri"] != "file://doc.txt" || item["text"] != "contents of file://doc.txt" {
		t.Fatalf("contents item = %v", item)
	}
}

func TestReadResourceTemplate(t *testing.T) {
	c := ctxWith(server.WithResources(userTemplate{}))
	resp, err := ReadResource{}.Handle(c, req(t, 1, "resources/read", map[string]any{"uri": "file://users/42"}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m := decodeResult(t, resp)
	item := m["contents"].([]any)[0].(map[string]any)
	if item["text"] != "42" {
		t.Fatalf("template var not injected: %v", item)
	}
}

func TestReadResourceMissingURI(t *testing.T) {
	c := ctxWith(server.WithResources(docResource{}))
	resp, _ := ReadResource{}.Handle(c, req(t, 1, "resources/read", map[string]any{}))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeResourceNotFound {
		t.Fatalf("expected resource not found, got %+v", resp.Error)
	}
}

func TestReadResourceNotFound(t *testing.T) {
	c := ctxWith(server.WithResources(docResource{}))
	resp, _ := ReadResource{}.Handle(c, req(t, 1, "resources/read", map[string]any{"uri": "file://nope"}))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeResourceNotFound {
		t.Fatalf("expected resource not found, got %+v", resp.Error)
	}
}

func TestListResources(t *testing.T) {
	c := ctxWith(server.WithResources(docResource{}, userTemplate{}))
	resp, _ := ListResources{}.Handle(c, req(t, 1, "resources/list", nil))
	m := decodeResult(t, resp)
	// Only the non-template resource is listed here.
	if len(m["resources"].([]any)) != 1 {
		t.Fatalf("resources = %v", m["resources"])
	}
	r := m["resources"].([]any)[0].(map[string]any)
	if r["uri"] != "file://doc.txt" {
		t.Fatalf("resource = %v", r)
	}
}

func TestListResourceTemplates(t *testing.T) {
	c := ctxWith(server.WithResources(docResource{}, userTemplate{}))
	resp, _ := ListResourceTemplates{}.Handle(c, req(t, 1, "resources/templates/list", nil))
	m := decodeResult(t, resp)
	if len(m["resourceTemplates"].([]any)) != 1 {
		t.Fatalf("templates = %v", m["resourceTemplates"])
	}
	tmpl := m["resourceTemplates"].([]any)[0].(map[string]any)
	if tmpl["uriTemplate"] != "file://users/{id}" {
		t.Fatalf("template = %v", tmpl)
	}
}

func TestListPrompts(t *testing.T) {
	c := ctxWith(server.WithPrompts(greetPrompt{}))
	resp, _ := ListPrompts{}.Handle(c, req(t, 1, "prompts/list", nil))
	m := decodeResult(t, resp)
	p := m["prompts"].([]any)[0].(map[string]any)
	if p["name"] != "greet" {
		t.Fatalf("prompt = %v", p)
	}
	args := p["arguments"].([]any)
	if len(args) != 1 {
		t.Fatalf("args = %v", args)
	}
}

func TestGetPrompt(t *testing.T) {
	c := ctxWith(server.WithPrompts(greetPrompt{}))
	resp, err := GetPrompt{}.Handle(c, req(t, 1, "prompts/get", map[string]any{
		"name":      "greet",
		"arguments": map[string]any{"name": "ada"},
	}))
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	m := decodeResult(t, resp)
	if m["description"] != "greets a person" {
		t.Fatalf("description = %v", m["description"])
	}
	msg := m["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "assistant" {
		t.Fatalf("role = %v", msg["role"])
	}
	content := msg["content"].(map[string]any)
	if content["text"] != "hello ada" {
		t.Fatalf("content = %v", content)
	}
}

func TestGetPromptMissingAndNotFound(t *testing.T) {
	c := ctxWith(server.WithPrompts(greetPrompt{}))
	resp, _ := GetPrompt{}.Handle(c, req(t, 1, "prompts/get", map[string]any{}))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected missing name error, got %+v", resp.Error)
	}
	resp2, _ := GetPrompt{}.Handle(c, req(t, 2, "prompts/get", map[string]any{"name": "nope"}))
	if resp2.Error == nil || resp2.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected not found, got %+v", resp2.Error)
	}
}

func TestCompletionNotSupported(t *testing.T) {
	c := ctxWith(server.WithPrompts(greetPrompt{}))
	resp, _ := CompletionComplete{}.Handle(c, req(t, 1, "completion/complete", map[string]any{
		"ref":      map[string]any{"type": "ref/prompt", "name": "greet"},
		"argument": map[string]any{"name": "name"},
	}))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("expected method not found, got %+v", resp.Error)
	}
}

func TestCompletionEmpty(t *testing.T) {
	c := ctxWith(
		server.WithPrompts(greetPrompt{}),
		server.WithCapability(server.CapabilityCompletions),
	)
	resp, err := CompletionComplete{}.Handle(c, req(t, 1, "completion/complete", map[string]any{
		"ref":      map[string]any{"type": "ref/prompt", "name": "greet"},
		"argument": map[string]any{"name": "name"},
	}))
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	m := decodeResult(t, resp)
	comp := m["completion"].(map[string]any)
	if comp["total"].(float64) != 0 || comp["hasMore"] != false {
		t.Fatalf("completion = %v", comp)
	}
}

func TestCompletionValidation(t *testing.T) {
	c := ctxWith(
		server.WithPrompts(greetPrompt{}),
		server.WithResources(docResource{}),
		server.WithCapability(server.CapabilityCompletions),
	)
	tests := []struct {
		name   string
		params map[string]any
		code   int
	}{
		{"missing ref", map[string]any{"argument": map[string]any{"name": "x"}}, jsonrpc.CodeInvalidParams},
		{"bad ref type", map[string]any{"ref": map[string]any{"type": "ref/bogus"}, "argument": map[string]any{"name": "x"}}, jsonrpc.CodeInvalidParams},
		{"prompt not found", map[string]any{"ref": map[string]any{"type": "ref/prompt", "name": "nope"}, "argument": map[string]any{"name": "x"}}, jsonrpc.CodeInvalidParams},
		{"resource not found", map[string]any{"ref": map[string]any{"type": "ref/resource", "uri": "file://nope"}, "argument": map[string]any{"name": "x"}}, jsonrpc.CodeInvalidParams},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _ := CompletionComplete{}.Handle(c, req(t, 1, "completion/complete", tt.params))
			if resp.Error == nil || resp.Error.Code != tt.code {
				t.Fatalf("expected code %d, got %+v", tt.code, resp.Error)
			}
		})
	}
}

// TestCompletionComplete_ResolvedRefMissingArgumentName asserts that when a
// reference resolves to a registered (but non-completable) primitive and the
// argument.name is absent, the handler returns the empty completion shape with
// no error, since the handler short-circuits non-completable
// primitives BEFORE inspecting argument.name.
func TestCompletionComplete_ResolvedRefMissingArgumentName(t *testing.T) {
	c := ctxWith(
		server.WithPrompts(greetPrompt{}),
		server.WithCapability(server.CapabilityCompletions),
	)
	params := map[string]any{
		"ref":      map[string]any{"type": "ref/prompt", "name": "greet"},
		"argument": map[string]any{},
	}
	resp, err := CompletionComplete{}.Handle(c, req(t, 1, "completion/complete", params))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("expected empty completion, got error %+v", resp.Error)
	}
	m := decodeResult(t, resp)
	comp, ok := m["completion"].(map[string]any)
	if !ok {
		t.Fatalf("missing completion object: %v", m)
	}
	if comp["total"].(float64) != 0 || comp["hasMore"].(bool) != false {
		t.Fatalf("expected empty completion, got %v", comp)
	}
}

func TestInitializeMethodHandler(t *testing.T) {
	c := ctxWith()
	resp, err := InitializeMethod{}.Handle(c, req(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"}))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	m := decodeResult(t, resp)
	if m["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", m["protocolVersion"])
	}
}

func TestDefaultMethodsComplete(t *testing.T) {
	m := defaultMethods()
	want := []string{
		"initialize", "ping", "tools/list", "tools/call",
		"resources/list", "resources/read", "resources/templates/list",
		"prompts/list", "prompts/get", "completion/complete",
	}
	for _, name := range want {
		if _, ok := m[name]; !ok {
			t.Fatalf("missing method %q", name)
		}
	}
	if len(m) != len(want) {
		t.Fatalf("method set size = %d want %d", len(m), len(want))
	}
}
