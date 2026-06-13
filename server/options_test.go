package server

import (
	"context"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
)

type stubResource struct {
	name, uri, mime string
}

func (r stubResource) Name() string        { return r.name }
func (r stubResource) Description() string { return "desc" }
func (r stubResource) URI() string         { return r.uri }
func (r stubResource) MimeType() string    { return r.mime }
func (r stubResource) Read(context.Context, *Request) (*Response, error) {
	return Text("body"), nil
}

type stubTemplate struct {
	stubResource
	tmpl string
}

func (r stubTemplate) URITemplate() string { return r.tmpl }

type stubPrompt struct{ name string }

func (p stubPrompt) Name() string                { return p.name }
func (p stubPrompt) Description() string         { return "desc" }
func (p stubPrompt) Arguments() []PromptArgument { return nil }
func (p stubPrompt) Handle(context.Context, *Request) (*Response, error) {
	return Text("ok"), nil
}

func TestOptions(t *testing.T) {
	s := New("demo", "1.0.0",
		WithInstructions("do things"),
		WithTitle("Demo"),
		WithWebsiteURL("https://example.com"),
		WithIcons(schema.NewIcon("icon.png")),
		WithTools(WeatherTool{}),
		WithResources(
			stubResource{name: "file", uri: "file://x", mime: "text/plain"},
			stubTemplate{stubResource: stubResource{name: "t", mime: "text/plain"}, tmpl: "file://u/{id}"},
		),
		WithPrompts(stubPrompt{name: "greet"}),
		WithPageSize(20),
		WithMaxPageSize(40),
	)

	if s.Instructions() != "do things" {
		t.Fatalf("instructions = %q", s.Instructions())
	}
	if len(s.Tools()) != 1 {
		t.Fatalf("tools = %d", len(s.Tools()))
	}
	if len(s.Resources()) != 1 {
		t.Fatalf("resources = %d", len(s.Resources()))
	}
	if len(s.ResourceTemplates()) != 1 {
		t.Fatalf("templates = %d", len(s.ResourceTemplates()))
	}
	if len(s.Prompts()) != 1 {
		t.Fatalf("prompts = %d", len(s.Prompts()))
	}
	if s.defaultPageSize != 20 || s.maxPageSize != 40 {
		t.Fatalf("page sizes = %d/%d", s.defaultPageSize, s.maxPageSize)
	}

	impl := s.implementation()
	m := impl.ToMap()
	if m["title"] != "Demo" || m["websiteUrl"] != "https://example.com" {
		t.Fatalf("implementation = %v", m)
	}
}

func TestOptionsIgnoreNilAndZero(t *testing.T) {
	s := New("d", "1",
		WithTools(nil),
		WithResources(nil),
		WithPrompts(nil),
		WithPageSize(0),
		WithMaxPageSize(-1),
		nil,
	)
	if len(s.Tools()) != 0 || len(s.Resources()) != 0 || len(s.Prompts()) != 0 {
		t.Fatal("nil primitives should be ignored")
	}
	if s.defaultPageSize != defaultPageSize || s.maxPageSize != defaultMaxPageSize {
		t.Fatal("zero/negative page sizes should be ignored")
	}
}

func TestWithCapability(t *testing.T) {
	s := New("d", "1",
		WithCapability(CapabilityCompletions),
		WithCapability("experimental.sampling", true),
		WithCapability("experimental.logging", false),
	)
	c := s.createContext(context.Background(), "", nil)
	if !c.HasCapability(CapabilityCompletions) {
		t.Fatal("completions capability not advertised")
	}
	exp, ok := c.Capabilities()["experimental"].(map[string]any)
	if !ok {
		t.Fatalf("experimental capability = %v", c.Capabilities()["experimental"])
	}
	if exp["sampling"] != true || exp["logging"] != false {
		t.Fatalf("nested caps = %v", exp)
	}
}

func TestWithProtocolVersions(t *testing.T) {
	s := New("d", "1", WithProtocolVersions("2025-06-18", "2024-11-05"))
	c := s.createContext(context.Background(), "", nil)
	versions := c.SupportedProtocolVersions()
	if len(versions) != 2 || versions[0] != "2025-06-18" {
		t.Fatalf("versions = %v", versions)
	}

	// Empty list is ignored.
	s2 := New("d", "1", WithProtocolVersions())
	if len(s2.createContext(context.Background(), "", nil).SupportedProtocolVersions()) != 4 {
		t.Fatal("empty version list should retain defaults")
	}
}
