package client

import "context"

// Tool is a tool advertised by the server (one entry of tools/list). When it was
// obtained through a Client it is bound to that client and can be invoked
// directly via Call.
type Tool struct {
	client *Client

	Name         string
	Title        string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Annotations  map[string]any
	Meta         map[string]any
}

// Call invokes the tool with the given arguments. It fails if the tool is not
// bound to a client (e.g. constructed by hand rather than returned from Tools).
func (t Tool) Call(ctx context.Context, arguments map[string]any) (*ToolResult, error) {
	if t.client == nil {
		return nil, newError("tool [" + t.Name + "] is not bound to a client")
	}
	return t.client.CallTool(ctx, t.Name, arguments)
}

// parseTool decodes a tools/list entry, binding it to client.
func parseTool(client *Client, payload map[string]any) (Tool, error) {
	name, _ := payload["name"].(string)
	if name == "" {
		return Tool{}, newError("invalid tool payload from server")
	}
	return Tool{
		client:       client,
		Name:         name,
		Title:        stringValue(payload, "title"),
		Description:  stringValue(payload, "description"),
		InputSchema:  mapValue(payload, "inputSchema"),
		OutputSchema: mapValue(payload, "outputSchema"),
		Annotations:  mapValue(payload, "annotations"),
		Meta:         mapValue(payload, "_meta"),
	}, nil
}

// Resource is a resource advertised by the server (one entry of resources/list).
type Resource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MimeType    string
	Size        *int64
	Annotations map[string]any
	Meta        map[string]any
}

// parseResource decodes a resources/list entry.
func parseResource(payload map[string]any) (Resource, error) {
	uri, _ := payload["uri"].(string)
	name, _ := payload["name"].(string)
	if uri == "" || name == "" {
		return Resource{}, newError("invalid resource payload from server")
	}
	r := Resource{
		URI:         uri,
		Name:        name,
		Title:       stringValue(payload, "title"),
		Description: stringValue(payload, "description"),
		MimeType:    stringValue(payload, "mimeType"),
		Annotations: mapValue(payload, "annotations"),
		Meta:        mapValue(payload, "_meta"),
	}
	if size, ok := payload["size"].(float64); ok {
		s := int64(size)
		r.Size = &s
	}
	return r, nil
}

// Prompt is a prompt advertised by the server (one entry of prompts/list).
type Prompt struct {
	Name        string
	Title       string
	Description string
	Arguments   []map[string]any
	Meta        map[string]any
}

// parsePrompt decodes a prompts/list entry.
func parsePrompt(payload map[string]any) (Prompt, error) {
	name, _ := payload["name"].(string)
	if name == "" {
		return Prompt{}, newError("invalid prompt payload from server")
	}
	p := Prompt{
		Name:        name,
		Title:       stringValue(payload, "title"),
		Description: stringValue(payload, "description"),
		Meta:        mapValue(payload, "_meta"),
	}
	if raw, ok := payload["arguments"].([]any); ok {
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				p.Arguments = append(p.Arguments, m)
			}
		}
	}
	return p, nil
}

// stringValue returns the string at key, or "".
func stringValue(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// mapValue returns the object at key, or nil.
func mapValue(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}
