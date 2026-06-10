package mcptest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/mcptest"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
	_ "github.com/velocitykode/velocity-mcp/server/methods" // installs the full method set
)

// addTool is a closure tool that adds two numbers, used across the tests.
func addTool() server.Tool {
	return server.NewTool("add", "Add two numbers").
		WithSchema(func(s *schema.Object) {
			s.Number("a").Required()
			s.Number("b").Required()
		}).
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			return server.Text(formatNumber(req.Float("a") + req.Float("b"))), nil
		})
}

// boomTool always returns a tool-level error result.
func boomTool() server.Tool {
	return server.NewTool("boom", "Always fails").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Error("kaboom"), nil
		})
}

func formatNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// greetingResource is a static text resource.
type greetingResource struct{}

func (greetingResource) Name() string        { return "greeting" }
func (greetingResource) Description() string { return "A friendly greeting" }
func (greetingResource) URI() string         { return "file://greeting.txt" }
func (greetingResource) MimeType() string    { return "text/plain" }
func (greetingResource) Read(_ context.Context, _ *server.Request) (*server.Response, error) {
	return server.NewResponse(content.NewText("hello world")), nil
}

// echoPrompt renders a prompt echoing its "topic" argument.
type echoPrompt struct{}

func (echoPrompt) Name() string        { return "echo" }
func (echoPrompt) Description() string { return "Echo a topic" }
func (echoPrompt) Arguments() []server.PromptArgument {
	return []server.PromptArgument{server.NewPromptArgument("topic", "The topic", true)}
}
func (echoPrompt) Handle(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("topic is " + req.String("topic")), nil
}

func demoServer() *server.Server {
	return server.New("demo", "1.0.0",
		server.WithInstructions("demo instructions"),
		server.WithTools(addTool(), boomTool()),
		server.WithResources(greetingResource{}),
		server.WithPrompts(echoPrompt{}),
	)
}

func TestServer_Initialize(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	res := ts.Initialize()

	res.AssertOk().
		AssertProtocolVersion("2025-06-18").
		AssertServerName("demo")

	if ts.SessionID() == "" {
		t.Fatal("Initialize should assign a session id")
	}
}

func TestServer_InitializeWith(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	res := ts.InitializeWith("2025-06-18", "custom-client", "9.9")
	res.AssertOk().AssertProtocolVersion("2025-06-18")
}

func TestServer_CallTool(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.CallTool("add", map[string]any{"a": 2, "b": 3})
	res.AssertOk().AssertText("5")
}

func TestServer_CallTool_NilArguments(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	// add with no args coerces missing numbers to 0 -> "0".
	res := ts.CallTool("add", nil)
	res.AssertOk().AssertText("0")
}

func TestServer_CallTool_ToolError(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.CallTool("boom", nil)
	res.AssertError("kaboom")
}

func TestServer_CallTool_UnknownTool(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.CallTool("does-not-exist", nil)
	res.AssertError().AssertErrorCode(jsonrpc.CodeInvalidParams)
}

func TestServer_ListTools(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.ListTools()
	res.AssertOk().
		AssertToolListed("add").
		AssertToolListed("boom").
		AssertToolNotListed("ghost").
		AssertToolCount(2)
}

func TestServer_ListResources(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.ListResources()
	res.AssertOk().AssertResourceListed("greeting")
}

func TestServer_ListPrompts(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.ListPrompts()
	res.AssertOk().AssertPromptListed("echo")
}

func TestServer_ReadResource(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.ReadResource("file://greeting.txt")
	res.AssertOk().AssertText("hello world")
}

func TestServer_GetPrompt(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.GetPrompt("echo", map[string]any{"topic": "weather"})
	res.AssertOk().AssertText("topic is weather")
}

func TestServer_GetPrompt_NilArguments(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize()

	res := ts.GetPrompt("echo", nil)
	res.AssertOk().AssertText("topic is ")
}

func TestServer_Ping(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	res := ts.Ping()
	res.AssertOk().AssertHasResponse()
}

func TestServer_Send_MethodNotFound(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	res := ts.Send("does/not/exist", nil)
	res.AssertError().AssertErrorCode(jsonrpc.CodeMethodNotFound)
}

func TestServer_WithContext(t *testing.T) {
	// WithContext threads the supplied context into Server.Handle, which the
	// server passes to the event dispatcher. Assert the dispatcher observes the
	// carried value (tool handlers themselves take a fresh context.Background in
	// the current method dispatch, so the dispatcher is the observable seam).
	type ctxKey struct{}
	seen := make(chan any, 1)
	srv := server.New("ctx", "1.0.0", server.WithTools(addTool()))
	srv.SetEventDispatcher(func(ctx context.Context, _ any) error {
		select {
		case seen <- ctx.Value(ctxKey{}):
		default:
		}
		return nil
	})
	ctx := context.WithValue(context.Background(), ctxKey{}, "carried")
	ts := mcptest.NewServer(t, srv).WithContext(ctx)
	ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertOk()

	if got := <-seen; got != "carried" {
		t.Fatalf("dispatcher context value = %v, want carried", got)
	}
}

func TestServer_WithContext_Nil(t *testing.T) {
	// A nil context must be tolerated (replaced with context.Background) rather
	// than threaded through and dereferenced. The nil is held in a variable so
	// the guard is exercised without a literal-nil lint flag.
	var ctx context.Context
	ts := mcptest.NewServer(t, demoServer()).WithContext(ctx)
	ts.Ping().AssertOk()
}

func TestServer_NilT(t *testing.T) {
	// A nil testing.TB must not panic: the helpers degrade to no-op failure
	// reporting so they remain usable outside a *testing.T (e.g. examples).
	ts := mcptest.NewServer(nil, demoServer())
	res := ts.Initialize()
	res.AssertOk()
	// An assertion that would fail simply does nothing when t is nil.
	res.AssertText("this is not present")
}

func TestServer_AssertResult(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	res := ts.Initialize()
	res.AssertResult("instructions", "demo instructions")
}

func TestServer_RawAndResult(t *testing.T) {
	ts := mcptest.NewServer(t, demoServer())
	res := ts.Initialize()
	if res.Raw() == nil {
		t.Fatal("Raw() should be non-nil for a request reply")
	}
	if res.Result() == nil {
		t.Fatal("Result() should be non-nil for a success reply")
	}
}
