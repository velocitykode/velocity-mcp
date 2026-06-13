package server

import (
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/content"
)

// Role identifies the author of a prompt message. Tool and resource results do
// not carry a role; prompt messages do.
type Role string

const (
	// RoleUser marks a message authored by the user. This is the default.
	RoleUser Role = "user"
	// RoleAssistant marks a message authored by the assistant.
	RoleAssistant Role = "assistant"
)

// Response is the result of handling a tool, resource, or prompt invocation:
// one or more content items, an optional role (for prompt messages), an isError
// flag (for tool-level error results), and optional structured content and
// metadata.
//
// Build responses with the package constructors (Text, NewResponse, Error) and
// the fluent With* methods. A nil *Response is treated by the method handlers as
// an empty (no content) result.
type Response struct {
	items      []content.Content
	role       Role
	isError    bool
	meta       map[string]any
	structured map[string]any
}

// NewResponse builds a Response carrying the given content items in order. At
// least one item is expected; an empty call yields a response with no content
// (a valid, if unusual, result).
func NewResponse(items ...content.Content) *Response {
	return &Response{items: append([]content.Content(nil), items...), role: RoleUser}
}

// Text builds a Response with a single text content item.
func Text(text string) *Response {
	return NewResponse(content.NewText(text))
}

// Error builds a tool-level error Response with a single text content item and
// the isError flag set. The message is returned to the client as the error
// result text, so callers must pass a client-safe message and never leak
// internal error detail.
func Error(message string) *Response {
	r := Text(message)
	r.isError = true
	return r
}

// Image builds a Response with a single image content item from raw (unencoded)
// bytes. An empty mimeType falls back to the image content default.
func Image(data []byte, mimeType string) *Response {
	return NewResponse(content.NewImage(data, mimeType))
}

// Audio builds a Response with a single audio content item from raw (unencoded)
// bytes. An empty mimeType falls back to the audio content default.
func Audio(data []byte, mimeType string) *Response {
	return NewResponse(content.NewAudio(data, mimeType))
}

// JSON builds a Response from an arbitrary value: the value is encoded as a
// text content item (so any client can display it) and, when it encodes to a
// JSON object, also attached as structuredContent for clients that consume the
// machine-readable form. An encoding error is returned to the caller.
func JSON(v any) (*Response, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	r := Text(string(b))
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil && obj != nil {
		r.structured = obj
	}
	return r, nil
}

// AsError marks the response as a tool-level error result and returns it for
// chaining.
func (r *Response) AsError() *Response {
	r.isError = true
	return r
}

// AsAssistant sets the response role to assistant (for prompt messages) and
// returns it for chaining.
func (r *Response) AsAssistant() *Response {
	r.role = RoleAssistant
	return r
}

// WithMeta merges the given key/value into the response _meta map and returns
// the response for chaining.
func (r *Response) WithMeta(key string, value any) *Response {
	if r.meta == nil {
		r.meta = make(map[string]any)
	}
	r.meta[key] = value
	return r
}

// WithStructuredContent attaches structured content to the response (surfaced
// under "structuredContent" in a tools/call result) and returns the response
// for chaining.
func (r *Response) WithStructuredContent(structured map[string]any) *Response {
	r.structured = structured
	return r
}

// IsError reports whether the response is a tool-level error result.
func (r *Response) IsError() bool { return r != nil && r.isError }

// Role returns the response role, defaulting to RoleUser.
func (r *Response) Role() Role {
	if r == nil || r.role == "" {
		return RoleUser
	}
	return r.role
}

// Contents returns the response's content items.
func (r *Response) Contents() []content.Content {
	if r == nil {
		return nil
	}
	return r.items
}

// Meta returns the response _meta map, or nil.
func (r *Response) Meta() map[string]any {
	if r == nil {
		return nil
	}
	return r.meta
}

// StructuredContent returns the response structured content, or nil.
func (r *Response) StructuredContent() map[string]any {
	if r == nil {
		return nil
	}
	return r.structured
}

// mergeMeta folds the response _meta and structured content into a result map
// produced by a method serializer: keys are only added when present and do
// not overwrite an existing key.
func (r *Response) mergeMeta(base map[string]any) map[string]any {
	if r == nil {
		return base
	}
	if len(r.meta) > 0 {
		if _, exists := base["_meta"]; !exists {
			m := make(map[string]any, len(r.meta))
			for k, v := range r.meta {
				m[k] = v
			}
			base["_meta"] = m
		}
	}
	if len(r.structured) > 0 {
		if _, exists := base["structuredContent"]; !exists {
			base["structuredContent"] = r.structured
		}
	}
	return base
}
