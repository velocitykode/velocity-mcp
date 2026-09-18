package client

import (
	"context"
	"time"
)

// Tool is a tool advertised by the server (one entry of tools/list). When it was
// obtained through a Client it is bound to that client and can be invoked
// directly via Call.
type Tool struct {
	client *Client

	// mirrored is the set of input properties this tool's schema asks to be
	// mirrored into request headers, and mirrorErr the reason the schema's
	// annotations were refused. A tool carrying the latter is never advertised
	// by Tools: its definition is invalid, so calling it could only fail.
	mirrored  mirroredParameters
	mirrorErr error

	// generation is the connection the listing that produced this value read it
	// over. A definition describes the server that stated it, so a call made
	// over a later connection reads the definition again rather than mirroring
	// what another server asked for.
	generation int64

	// staleAfter is the moment the page that carried this definition stops
	// being fresh, and changes the count of changes the server had announced to
	// its catalogue when the listing set out. A value may be held for as long
	// as the caller likes, which neither the lifetime the server gave the
	// definition nor what it has announced since says anything about: a call
	// made past either reads the definition again rather than mirroring what
	// the server asked for once.
	staleAfter time.Time
	changes    int64

	Name         string
	Title        string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Annotations  map[string]any
	Meta         map[string]any
}

// Call invokes the tool with the given arguments, mirroring the input
// properties its schema annotates into request headers. It fails if the tool is
// not bound to a client (e.g. constructed by hand rather than returned from
// Tools).
//
// An optional Continuation repeats a call the server left unfinished, carrying
// the inputs it asked for; see UnfinishedResultError.
func (t Tool) Call(ctx context.Context, arguments map[string]any, continuation ...Continuation) (*ToolResult, error) {
	if t.client == nil {
		return nil, newError("tool [" + t.Name + "] is not bound to a client")
	}
	// The definition this value carries is the one the listing that produced it
	// read, over the connection that listing stood on, which the server may
	// have changed since and which a later handshake may have replaced. The
	// call is therefore weighed against a held definition rather than a freshly
	// read one.
	return t.client.callTool(ctx, t.Name, arguments, t.client.held(t.stated(), t.generation), continuation)
}

// stated returns what the listing that produced this value stated for the tool.
func (t Tool) stated() statedDefinition {
	return statedDefinition{params: t.mirrored, staleAfter: t.staleAfter, changes: t.changes}
}

// parseTool decodes a tools/list entry, binding it to client. A schema whose
// header annotations are invalid does not fail the decode: the reason is
// recorded on the tool, and the caller decides what to do with an entry it
// cannot use.
func parseTool(client *Client, payload map[string]any) (Tool, error) {
	name, _ := payload["name"].(string)
	if name == "" {
		return Tool{}, newError("invalid tool payload from server")
	}
	tool := Tool{
		client:       client,
		Name:         name,
		Title:        stringValue(payload, "title"),
		Description:  stringValue(payload, "description"),
		InputSchema:  mapValue(payload, "inputSchema"),
		OutputSchema: mapValue(payload, "outputSchema"),
		Annotations:  mapValue(payload, "annotations"),
		Meta:         mapValue(payload, "_meta"),
	}
	tool.mirrored, tool.mirrorErr = parseMirroredParameters(tool.InputSchema)
	return tool, nil
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
