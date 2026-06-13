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

	m := map[string]any{
		"name":        t.Name(),
		"title":       serverTitle(t.Name(), t),
		"description": t.Description(),
		"inputSchema": input,
		"annotations": toolAnnotations(t),
	}
	if out, ok := toolOutputSchema(t); ok {
		m["outputSchema"] = out
	}
	return m
}

// toolOutputSchema returns a tool's output schema and whether it declares one.
// A tool opts in by implementing server.StructuredOutput and returning true
// from OutputSchema; the rendered schema always carries a "properties" object
// (empty when none were declared), matching the inputSchema shape.
func toolOutputSchema(t server.Tool) (map[string]any, bool) {
	so, ok := t.(server.StructuredOutput)
	if !ok {
		return nil, false
	}
	obj := schema.NewObject()
	if !so.OutputSchema(obj) {
		return nil, false
	}
	out := obj.ToMap()
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	return out, true
}

// toolAnnotations returns a tool's behavior-hint annotations object, delegating
// to the tool when it implements server.Annotated and otherwise yielding an
// empty object. Follows the MCP tools/list annotations handling, where
// unset hints are omitted and an empty set serializes as "{}".
func toolAnnotations(t server.Tool) map[string]any {
	if a, ok := t.(server.Annotated); ok {
		return a.Annotations().ToMap()
	}
	return map[string]any{}
}

// resourceToMap renders a non-template resource to its resources/list wire
// shape.
func resourceToMap(r server.Resource) map[string]any {
	m := map[string]any{
		"name":        r.Name(),
		"title":       serverTitle(r.Name(), r),
		"description": r.Description(),
		"mimeType":    r.MimeType(),
		"uri":         r.URI(),
	}
	return withAppMeta(withResourceAnnotations(m, r), r)
}

// templateToMap renders a URI-template resource to its resources/templates/list
// wire shape (uriTemplate instead of uri).
func templateToMap(r server.URITemplate) map[string]any {
	m := map[string]any{
		"name":        r.Name(),
		"title":       serverTitle(r.Name(), r),
		"description": r.Description(),
		"mimeType":    r.MimeType(),
		"uriTemplate": r.URITemplate(),
	}
	return withAppMeta(withResourceAnnotations(m, r), r)
}

// withAppMeta adds the MCP UI app metadata to m under "_meta.ui" when r is an
// app resource (implements server.AppResource) and the metadata is non-empty.
// The key is nested under "_meta" without disturbing any other _meta keys, so a
// host that ignores the extension sees an ordinary resource entry.
func withAppMeta(m map[string]any, r server.Resource) map[string]any {
	a, ok := r.(server.AppResource)
	if !ok {
		return m
	}
	uiMeta := a.AppMeta().ToMap()
	if len(uiMeta) == 0 {
		return m
	}
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["ui"] = uiMeta
	m["_meta"] = meta
	return m
}

// withResourceAnnotations adds the "annotations" key to m when r implements
// server.ResourceAnnotated and carries at least one set annotation. Unlike tool
// annotations, the key is omitted entirely when empty, following the MCP
// resources/list and resources/templates/list shapes where annotations are
// optional.
func withResourceAnnotations(m map[string]any, r server.Resource) map[string]any {
	a, ok := r.(server.ResourceAnnotated)
	if !ok {
		return m
	}
	if ann := a.Annotations().ToMap(); len(ann) > 0 {
		m["annotations"] = ann
	}
	return m
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
