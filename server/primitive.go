package server

import (
	"context"
	"reflect"
	"strings"

	"github.com/velocitykode/velocity/str"

	"github.com/velocitykode/velocity-mcp/schema"
)

// Tool is a server primitive an MCP client can invoke (tools/call).
// Implementations describe their input arguments via Schema and run via Handle.
//
// Name must be kebab-case and unique within a server; DefaultName derives a
// suitable name from a Go type. Description is shown to clients in tools/list.
type Tool interface {
	// Name is the tool's unique, kebab-case identifier.
	Name() string
	// Description is the human-readable tool description.
	Description() string
	// Schema populates s with the tool's input argument schema.
	Schema(s *schema.Object)
	// Handle runs the tool against the request arguments and returns a
	// response. A non-nil error is treated as a tool failure (see
	// methods.CallTool); to return a tool-level error result without failing
	// the call, return a response built with Response.AsError and a nil error.
	Handle(ctx context.Context, req *Request) (*Response, error)
}

// Titled is implemented by primitives that expose a human-friendly title
// distinct from their name.
type Titled interface {
	Title() string
}

// ToolAnnotations carries the MCP tool behavior hints surfaced in tools/list
// under the "annotations" object. Each field is a pointer so an unset hint is
// omitted from the wire (clients apply the spec default), while an explicit
// false is still transmitted. The pointer is necessary because destructiveHint
// and openWorldHint default to true, so a plain bool with omitempty would
// silently drop a deliberate false.
//
// All four are hints only: per the MCP spec, clients must treat them as
// untrusted unless the server is trusted, and they never guarantee behavior.
type ToolAnnotations struct {
	// ReadOnly hints the tool does not modify its environment (readOnlyHint;
	// spec default false).
	ReadOnly *bool
	// Destructive hints the tool may perform destructive updates rather than
	// only additive ones; meaningful only when ReadOnly is false
	// (destructiveHint; spec default true).
	Destructive *bool
	// Idempotent hints repeated calls with the same arguments have no
	// additional effect; meaningful only when ReadOnly is false
	// (idempotentHint; spec default false).
	Idempotent *bool
	// OpenWorld hints the tool may interact with an open world of external
	// entities rather than a closed domain (openWorldHint; spec default true).
	OpenWorld *bool
}

// ToMap renders the hints to their MCP wire shape, omitting any unset hint. An
// all-unset value yields an empty map, which callers emit as an empty
// "annotations" object.
func (a ToolAnnotations) ToMap() map[string]any {
	m := map[string]any{}
	if a.ReadOnly != nil {
		m["readOnlyHint"] = *a.ReadOnly
	}
	if a.Destructive != nil {
		m["destructiveHint"] = *a.Destructive
	}
	if a.Idempotent != nil {
		m["idempotentHint"] = *a.Idempotent
	}
	if a.OpenWorld != nil {
		m["openWorldHint"] = *a.OpenWorld
	}
	return m
}

// Annotated is implemented by tools that expose behavior-hint annotations in
// tools/list. A tool that does not implement it reports no hints (an empty
// annotations object).
type Annotated interface {
	Annotations() ToolAnnotations
}

// Resource is a readable server primitive identified by a URI (resources/read).
// A resource whose URI is a template (contains "{var}" placeholders) is listed
// under resources/templates/list instead of resources/list; implement
// URITemplate to opt in.
type Resource interface {
	// Name is the resource's unique, kebab-case identifier.
	Name() string
	// Description is the human-readable resource description.
	Description() string
	// URI is the resource URI (or URI template for templated resources).
	URI() string
	// MimeType is the resource's MIME type (e.g. "text/plain").
	MimeType() string
	// Read returns the resource contents for the requested URI. The concrete
	// URI requested is available via req.URI(); for templated resources the
	// extracted template variables are available as request arguments.
	Read(ctx context.Context, req *Request) (*Response, error)
}

// URITemplate is implemented by resources whose URI is an RFC 6570-style
// template (e.g. "file://users/{id}"). Such resources are reported under
// resources/templates/list.
//
// A resource may implement this as a marker (URITemplate() returning its URI)
// or carry richer matching; the server treats any resource implementing this
// interface as a template and matches concrete read URIs against it via
// MatchURITemplate.
type URITemplate interface {
	Resource
	// URITemplate returns the template string for the resource.
	URITemplate() string
}

// Prompt is a server primitive that produces prompt messages (prompts/get).
type Prompt interface {
	// Name is the prompt's unique, kebab-case identifier.
	Name() string
	// Description is the human-readable prompt description.
	Description() string
	// Arguments declares the prompt's accepted arguments for prompts/list.
	Arguments() []PromptArgument
	// Handle produces the prompt response from the supplied arguments.
	Handle(ctx context.Context, req *Request) (*Response, error)
}

// PromptArgument describes a single declared argument of a Prompt.
type PromptArgument struct {
	// Name is the argument name.
	Name string
	// Description is the human-readable argument description.
	Description string
	// Required reports whether the argument must be supplied.
	Required bool
}

// NewPromptArgument constructs a PromptArgument.
func NewPromptArgument(name, description string, required bool) PromptArgument {
	return PromptArgument{Name: name, Description: description, Required: required}
}

// ToMap renders the argument to its MCP wire shape.
func (a PromptArgument) ToMap() map[string]any {
	return map[string]any{
		"name":        a.Name,
		"description": a.Description,
		"required":    a.Required,
	}
}

// DefaultName derives a kebab-case primitive name from a Go value's type name.
// A pointer is dereferenced to its element type, and any package qualifier is
// dropped, so *myapp.WeatherTool yields "weather-tool".
//
// It uses velocity's str.Kebab so naming is consistent with the rest of the
// framework. An anonymous or unnamed type yields "".
func DefaultName(v any) string {
	if v == nil {
		return ""
	}
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return ""
	}
	name := t.Name()
	if name == "" {
		return ""
	}
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	return str.Kebab(name)
}

// titleOf returns a primitive's title: its Title() when it implements Titled,
// otherwise a headline derived from its name via velocity's str.Headline.
func titleOf(name string, p any) string {
	if t, ok := p.(Titled); ok {
		if v := t.Title(); v != "" {
			return v
		}
	}
	return str.Headline(name)
}
