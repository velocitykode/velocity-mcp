package methods

import (
	"github.com/velocitykode/velocity/str"

	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// toolToMap renders a tool to its tools/list wire shape: name, title,
// description, inputSchema, and an annotations object (always present, empty by
// default). The input schema always carries a "properties" object even when
// empty.
func toolToMap(t server.Tool) map[string]any {
	obj := schema.NewObject()
	t.Schema(obj)
	input := obj.ToMap()
	if _, ok := input["properties"]; !ok {
		input["properties"] = map[string]any{}
	}

	return map[string]any{
		"name":        t.Name(),
		"title":       serverTitle(t.Name(), t),
		"description": t.Description(),
		"inputSchema": input,
		"annotations": map[string]any{},
	}
}

// resourceToMap renders a non-template resource to its resources/list wire
// shape.
func resourceToMap(r server.Resource) map[string]any {
	return map[string]any{
		"name":        r.Name(),
		"title":       serverTitle(r.Name(), r),
		"description": r.Description(),
		"mimeType":    r.MimeType(),
		"uri":         r.URI(),
	}
}

// templateToMap renders a URI-template resource to its resources/templates/list
// wire shape (uriTemplate instead of uri).
func templateToMap(r server.URITemplate) map[string]any {
	return map[string]any{
		"name":        r.Name(),
		"title":       serverTitle(r.Name(), r),
		"description": r.Description(),
		"mimeType":    r.MimeType(),
		"uriTemplate": r.URITemplate(),
	}
}

// promptToMap renders a prompt to its prompts/list wire shape.
func promptToMap(p server.Prompt) map[string]any {
	args := p.Arguments()
	out := make([]map[string]any, 0, len(args))
	for _, a := range args {
		out = append(out, a.ToMap())
	}
	return map[string]any{
		"name":        p.Name(),
		"title":       serverTitle(p.Name(), p),
		"description": p.Description(),
		"arguments":   out,
	}
}

// serverTitle returns a primitive's title, delegating to the primitive's Title
// method when it implements server.Titled, otherwise deriving a headline from
// the name.
func serverTitle(name string, p any) string {
	if t, ok := p.(server.Titled); ok {
		if v := t.Title(); v != "" {
			return v
		}
	}
	return str.Headline(name)
}
