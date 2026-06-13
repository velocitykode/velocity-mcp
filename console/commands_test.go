package console

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// inventoryServer builds a server with one of each primitive for inspect tests.
type tmplResource struct{}

func (tmplResource) Name() string        { return "user" }
func (tmplResource) Description() string { return "a user" }
func (tmplResource) URI() string         { return "velocity://users/{id}" }
func (tmplResource) URITemplate() string { return "velocity://users/{id}" }
func (tmplResource) MimeType() string    { return "application/json" }
func (tmplResource) Read(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text(req.String("id")), nil
}

type docResource struct{}

func (docResource) Name() string        { return "doc" }
func (docResource) Description() string { return "a doc" }
func (docResource) URI() string         { return "velocity://doc" }
func (docResource) MimeType() string    { return "text/plain" }
func (docResource) Read(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("x"), nil
}

type greetPrompt struct{}

func (greetPrompt) Name() string                       { return "greet" }
func (greetPrompt) Description() string                { return "greets" }
func (greetPrompt) Arguments() []server.PromptArgument { return nil }
func (greetPrompt) Handle(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("hi"), nil
}

func inventoryServer() *server.Server {
	tool := server.NewTool("weather", "get weather").
		WithSchema(func(s *schema.Object) { s.String("city") })
	return server.New("demo", "1.2.3",
		server.WithInstructions("Use these tools."),
		server.WithTools(tool),
		server.WithResources(docResource{}, tmplResource{}),
		server.WithPrompts(greetPrompt{}),
	)
}

func TestServerCommandsRegistered(t *testing.T) {
	cmds := ServerCommands(inventoryServer())
	got := map[string]bool{}
	for _, c := range cmds {
		got[c.Name()] = true
		if c.Description() == "" {
			t.Fatalf("%s has no description", c.Name())
		}
	}
	for _, want := range []string{"mcp:start", "mcp:inspect"} {
		if !got[want] {
			t.Fatalf("missing command %q (have %v)", want, got)
		}
	}
}

func TestInspectInventory(t *testing.T) {
	var buf bytes.Buffer
	if err := writeInventory(&buf, inventoryServer()); err != nil {
		t.Fatalf("writeInventory: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"demo 1.2.3",
		"Use these tools.",
		"Tools (1):",
		"weather - get weather",
		"Resources (1):",
		"doc [velocity://doc]",
		"Resource templates (1):",
		"user [velocity://users/{id}]",
		"Prompts (1):",
		"greet - greets",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inventory missing %q in:\n%s", want, out)
		}
	}
}

func TestInspectEmptyServer(t *testing.T) {
	var buf bytes.Buffer
	if err := writeInventory(&buf, server.New("bare", "0.1.0")); err != nil {
		t.Fatalf("writeInventory: %v", err)
	}
	out := buf.String()
	// Every section reports (none) when empty.
	if strings.Count(out, "(none)") != 4 {
		t.Fatalf("expected 4 (none) sections, got:\n%s", out)
	}
}

func TestInspectHandleWritesToConfiguredWriter(t *testing.T) {
	var buf bytes.Buffer
	cmd := inspectCommand{srv: inventoryServer(), out: &buf}
	if err := cmd.Handle(nil, nil); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !strings.Contains(buf.String(), "demo 1.2.3") {
		t.Fatalf("inspect Handle wrote nothing useful:\n%s", buf.String())
	}
}
