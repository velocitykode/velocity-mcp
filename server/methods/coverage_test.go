package methods

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity/validation"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// titledTool exercises the Titled branch of serverTitle.
type titledTool struct{}

func (titledTool) Name() string          { return "kebab-name" }
func (titledTool) Title() string         { return "Explicit Title" }
func (titledTool) Description() string   { return "d" }
func (titledTool) Schema(*schema.Object) {}
func (titledTool) Handle(context.Context, *server.Request) (*server.Response, error) {
	return server.Text("ok"), nil
}

func TestServerTitleVariants(t *testing.T) {
	c := ctxWith(server.WithTools(titledTool{}, echoTool()))
	resp, _ := ListTools{}.Handle(c, req(t, 1, "tools/list", nil))
	m := decodeResult(t, resp)
	tools := m["tools"].([]any)

	byName := map[string]map[string]any{}
	for _, x := range tools {
		tm := x.(map[string]any)
		byName[tm["name"].(string)] = tm
	}
	if byName["kebab-name"]["title"] != "Explicit Title" {
		t.Fatalf("explicit title = %v", byName["kebab-name"]["title"])
	}
	// echo has no Title(): a headline is derived from the name.
	if byName["echo"]["title"] != "Echo" {
		t.Fatalf("derived title = %v", byName["echo"]["title"])
	}
}

// metaTool returns a response carrying _meta and structured content so the
// result-merging branches are exercised.
func metaTool() server.Tool {
	return server.NewTool("meta", "returns meta").
		HandleFunc(func(ctx context.Context, r *server.Request) (*server.Response, error) {
			return server.Text("body").
				WithMeta("trace", "abc").
				WithStructuredContent(map[string]any{"k": 1}), nil
		})
}

func TestCallToolMergesMetaAndStructured(t *testing.T) {
	c := ctxWith(server.WithTools(metaTool()))
	resp, err := CallTool{}.Handle(c, req(t, 1, "tools/call", map[string]any{"name": "meta"}))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	m := decodeResult(t, resp)
	meta := m["_meta"].(map[string]any)
	if meta["trace"] != "abc" {
		t.Fatalf("_meta = %v", meta)
	}
	sc := m["structuredContent"].(map[string]any)
	if sc["k"].(float64) != 1 {
		t.Fatalf("structuredContent = %v", sc)
	}
}

// validatingPrompt fails validation to exercise the GetPrompt error-result path.
type validatingPrompt struct{}

func (validatingPrompt) Name() string                       { return "vp" }
func (validatingPrompt) Description() string                { return "validates" }
func (validatingPrompt) Arguments() []server.PromptArgument { return nil }
func (validatingPrompt) Handle(ctx context.Context, r *server.Request) (*server.Response, error) {
	if err := r.Validate(validation.Rules{"name": {"required"}}); err != nil {
		return nil, err
	}
	return server.Text("ok"), nil
}

func TestGetPromptValidationErrorResult(t *testing.T) {
	c := ctxWith(server.WithPrompts(validatingPrompt{}))
	resp, err := GetPrompt{}.Handle(c, req(t, 1, "prompts/get", map[string]any{"name": "vp", "arguments": map[string]any{}}))
	if err != nil {
		t.Fatalf("validation should not be a Go error: %v", err)
	}
	m := decodeResult(t, resp)
	msg := m["messages"].([]any)[0].(map[string]any)
	content := msg["content"].(map[string]any)
	text, _ := content["text"].(string)
	if !strings.HasPrefix(text, "Invalid params:") {
		t.Fatalf("expected an Invalid params message, got %q", text)
	}
}

// failingPrompt returns an unexpected error so GetPrompt propagates it.
type failingPrompt struct{}

func (failingPrompt) Name() string                       { return "fp" }
func (failingPrompt) Description() string                { return "fails" }
func (failingPrompt) Arguments() []server.PromptArgument { return nil }
func (failingPrompt) Handle(context.Context, *server.Request) (*server.Response, error) {
	return nil, context.DeadlineExceeded
}

func TestGetPromptUnexpectedErrorPropagates(t *testing.T) {
	c := ctxWith(server.WithPrompts(failingPrompt{}))
	_, err := GetPrompt{}.Handle(c, req(t, 1, "prompts/get", map[string]any{"name": "fp"}))
	if err == nil {
		t.Fatal("expected propagated error")
	}
}

// validatingResource exercises the ReadResource validation error-result path.
type validatingResource struct{}

func (validatingResource) Name() string        { return "vr" }
func (validatingResource) Description() string { return "validates" }
func (validatingResource) URI() string         { return "file://vr" }
func (validatingResource) MimeType() string    { return "text/plain" }
func (validatingResource) Read(ctx context.Context, r *server.Request) (*server.Response, error) {
	if err := r.Validate(validation.Rules{"x": {"required"}}); err != nil {
		return nil, err
	}
	return server.Text("ok"), nil
}

func TestReadResourceValidationErrorResult(t *testing.T) {
	c := ctxWith(server.WithResources(validatingResource{}))
	resp, err := ReadResource{}.Handle(c, req(t, 1, "resources/read", map[string]any{"uri": "file://vr"}))
	if err != nil {
		t.Fatalf("validation should not be a Go error: %v", err)
	}
	m := decodeResult(t, resp)
	item := m["contents"].([]any)[0].(map[string]any)
	text, _ := item["text"].(string)
	if !strings.HasPrefix(text, "Invalid params:") {
		t.Fatalf("expected an Invalid params message, got %q", text)
	}
}

// failingResource returns an unexpected error so ReadResource propagates it.
type failingResource struct{}

func (failingResource) Name() string        { return "frs" }
func (failingResource) Description() string { return "fails" }
func (failingResource) URI() string         { return "file://frs" }
func (failingResource) MimeType() string    { return "text/plain" }
func (failingResource) Read(context.Context, *server.Request) (*server.Response, error) {
	return nil, context.DeadlineExceeded
}

func TestReadResourceUnexpectedErrorPropagates(t *testing.T) {
	c := ctxWith(server.WithResources(failingResource{}))
	_, err := ReadResource{}.Handle(c, req(t, 1, "resources/read", map[string]any{"uri": "file://frs"}))
	if err == nil {
		t.Fatal("expected propagated error")
	}
}

func TestCompletionResourceRefSuccess(t *testing.T) {
	c := ctxWith(
		server.WithResources(docResource{}),
		server.WithCapability(server.CapabilityCompletions),
	)
	resp, err := CompletionComplete{}.Handle(c, req(t, 1, "completion/complete", map[string]any{
		"ref":      map[string]any{"type": "ref/resource", "uri": "file://doc.txt"},
		"argument": map[string]any{"name": "q"},
	}))
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	m := decodeResult(t, resp)
	if _, ok := m["completion"]; !ok {
		t.Fatalf("missing completion: %v", m)
	}
}

func TestParamsIntValue(t *testing.T) {
	// Exercise intValue across float64, json.Number, and int.
	raw, _ := json.Marshal(map[string]any{"a": 5})
	p := decode(&jsonrpc.Request{Params: raw})
	if p.intValue("a") != 5 {
		t.Fatalf("float64 path = %d", p.intValue("a"))
	}

	pn := params{"n": json.Number("9"), "i": 3, "bad": "x"}
	if pn.intValue("n") != 9 {
		t.Fatalf("json.Number path = %d", pn.intValue("n"))
	}
	if pn.intValue("i") != 3 {
		t.Fatalf("int path = %d", pn.intValue("i"))
	}
	if pn.intValue("bad") != 0 || pn.intValue("missing") != 0 {
		t.Fatal("non-numeric/missing should be 0")
	}
}

func TestParamsIntPtr(t *testing.T) {
	// Absent and non-numeric keys yield nil so callers can distinguish an
	// omitted per_page from an explicit 0.
	p := params{"f": float64(5), "n": json.Number("9"), "i": 3, "z": float64(0), "bad": "x", "badnum": json.Number("nope")}
	mustVal := func(name string, got *int, want int) {
		if got == nil {
			t.Fatalf("%s: got nil, want %d", name, want)
		}
		if *got != want {
			t.Fatalf("%s: got %d, want %d", name, *got, want)
		}
	}
	mustVal("float64", p.intPtr("f"), 5)
	mustVal("json.Number", p.intPtr("n"), 9)
	mustVal("int", p.intPtr("i"), 3)
	mustVal("explicit zero", p.intPtr("z"), 0)
	if p.intPtr("missing") != nil {
		t.Fatal("absent key should be nil")
	}
	if p.intPtr("bad") != nil {
		t.Fatal("non-numeric value should be nil")
	}
	if p.intPtr("badnum") != nil {
		t.Fatal("unparseable json.Number should be nil")
	}
}

func TestDecodeEmptyAndBadParams(t *testing.T) {
	if len(decode(nil)) != 0 {
		t.Fatal("nil request should decode to empty params")
	}
	if len(decode(&jsonrpc.Request{Params: json.RawMessage("not json")})) != 0 {
		t.Fatal("bad params should decode to empty")
	}
	// Non-object params decode to empty.
	if len(decode(&jsonrpc.Request{Params: json.RawMessage("[1,2]")})) != 0 {
		t.Fatal("array params should decode to empty")
	}
}

func TestPromptResultNilResponse(t *testing.T) {
	// A nil response yields an empty messages array (not null).
	m, err := promptResult("desc", nil)
	if err != nil {
		t.Fatalf("promptResult: %v", err)
	}
	if msgs, ok := m["messages"].([]any); !ok || len(msgs) != 0 {
		t.Fatalf("messages = %v", m["messages"])
	}
}

func TestResourceResultResourceLinkNotAllowed(t *testing.T) {
	// A ResourceLink cannot be used in a resource context: resourceResult
	// surfaces the conversion error.
	resp := server.NewResponse(content.NewResourceLink("file://x", "x"))
	if _, err := resourceResult("file://x", "text/plain", resp); err == nil {
		t.Fatal("expected ErrNotAllowed from resource serialization")
	}
}

func TestValidationMessageFallback(t *testing.T) {
	// A non-validation error yields the generic fallback message.
	if got := validationMessage(context.DeadlineExceeded); got != "The given data was invalid." {
		t.Fatalf("fallback = %q", got)
	}
}
