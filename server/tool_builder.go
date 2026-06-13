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
	annotations ToolAnnotations
}

// Compile-time assertions that *ToolBuilder satisfies the Tool, Titled, and
// Annotated interfaces.
var (
	_ Tool      = (*ToolBuilder)(nil)
	_ Titled    = (*ToolBuilder)(nil)
	_ Annotated = (*ToolBuilder)(nil)
)

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

// WithReadOnlyHint sets the readOnlyHint annotation and returns the builder.
// Pass true when the tool does not modify its environment.
func (t *ToolBuilder) WithReadOnlyHint(v bool) *ToolBuilder {
	t.annotations.ReadOnly = &v
	return t
}

// WithDestructiveHint sets the destructiveHint annotation and returns the
// builder. Pass true when the tool may perform destructive (non-additive)
// updates. Meaningful only alongside WithReadOnlyHint(false).
func (t *ToolBuilder) WithDestructiveHint(v bool) *ToolBuilder {
	t.annotations.Destructive = &v
	return t
}

// WithIdempotentHint sets the idempotentHint annotation and returns the
// builder. Pass true when repeated calls with the same arguments have no
// additional effect. Meaningful only alongside WithReadOnlyHint(false).
func (t *ToolBuilder) WithIdempotentHint(v bool) *ToolBuilder {
	t.annotations.Idempotent = &v
	return t
}

// WithOpenWorldHint sets the openWorldHint annotation and returns the builder.
// Pass true when the tool may interact with an open world of external entities.
func (t *ToolBuilder) WithOpenWorldHint(v bool) *ToolBuilder {
	t.annotations.OpenWorld = &v
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

// Annotations implements Annotated, returning the configured behavior hints.
func (t *ToolBuilder) Annotations() ToolAnnotations { return t.annotations }

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
