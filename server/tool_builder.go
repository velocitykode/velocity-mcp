package server

import (
	"context"

	"github.com/velocitykode/velocity-mcp/schema"
)

// ToolBuilder is a closure-based Tool implementation: a tool defined inline via
// a name, description, schema callback, and handler func rather than a struct
// implementing the Tool interface. It mirrors the ergonomics of registering
// tools as closures.
//
// Build one with NewTool and configure it fluently with WithSchema and
// HandleFunc. A ToolBuilder with no HandleFunc returns an error result when
// invoked, so a misconfigured tool fails cleanly instead of panicking.
type ToolBuilder struct {
	name        string
	description string
	schemaFn    func(s *schema.Object)
	handleFn    func(ctx context.Context, req *Request) (*Response, error)
	title       string
}

// Compile-time assertion that *ToolBuilder satisfies the Tool interface.
var _ Tool = (*ToolBuilder)(nil)

// NewTool starts building a closure tool with the given kebab-case name and
// description.
func NewTool(name, description string) *ToolBuilder {
	return &ToolBuilder{name: name, description: description}
}

// WithSchema sets the callback that describes the tool's input arguments and
// returns the builder for chaining.
func (t *ToolBuilder) WithSchema(fn func(s *schema.Object)) *ToolBuilder {
	t.schemaFn = fn
	return t
}

// WithTitle sets a human-friendly title for the tool and returns the builder.
func (t *ToolBuilder) WithTitle(title string) *ToolBuilder {
	t.title = title
	return t
}

// HandleFunc sets the tool's handler and returns the builder for chaining.
func (t *ToolBuilder) HandleFunc(fn func(ctx context.Context, req *Request) (*Response, error)) *ToolBuilder {
	t.handleFn = fn
	return t
}

// Name returns the tool name.
func (t *ToolBuilder) Name() string { return t.name }

// Description returns the tool description.
func (t *ToolBuilder) Description() string { return t.description }

// Title implements Titled when a title was set.
func (t *ToolBuilder) Title() string { return t.title }

// Schema invokes the configured schema callback, if any.
func (t *ToolBuilder) Schema(s *schema.Object) {
	if t.schemaFn != nil {
		t.schemaFn(s)
	}
}

// Handle invokes the configured handler. When no handler was set it returns a
// tool-level error result (never a panic), so a half-built tool degrades
// gracefully.
func (t *ToolBuilder) Handle(ctx context.Context, req *Request) (*Response, error) {
	if t.handleFn == nil {
		return Error("Tool has no handler."), nil
	}
	return t.handleFn(ctx, req)
}
