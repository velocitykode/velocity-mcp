package content

import (
	"encoding/json"
	"errors"
)

// ErrNotAllowed is returned by a context conversion (ToTool, ToPrompt, or
// ToResource) when the concrete content type may not be used in that context.
// For example Blob content may not appear in tools or prompts, and ResourceLink
// content may not appear in resources.
var ErrNotAllowed = errors.New("content: type not allowed in this context")

// Content is the common interface implemented by every MCP content type.
//
// A content value marshals to its default MCP wire shape via MarshalJSON (the
// shape used in tool results and prompt messages, e.g. {"type":"text",...}).
// Because the MCP protocol uses a different shape for the same content
// depending on whether it appears in a tool result, a prompt message, or a
// resource read, the three context-specific conversions return a plain
// map[string]any that the server serializes:
//
//   - ToTool     - shape for a tools/call result content item.
//   - ToPrompt   - shape for a prompts/get message content item.
//   - ToResource - shape for a resources/read contents item (carries uri/mimeType).
//
// A conversion returns ErrNotAllowed when the type is invalid in that context.
type Content interface {
	json.Marshaler

	// ToTool returns the wire shape for a tools/call result content item.
	// uri and mimeType describe the owning primitive when the content needs
	// them (resource-backed shapes); they are ignored by tool-only shapes.
	ToTool() (map[string]any, error)

	// ToPrompt returns the wire shape for a prompts/get message content item.
	ToPrompt() (map[string]any, error)

	// ToResource returns the wire shape for a resources/read contents item.
	// uri and mimeType are those of the owning resource; they are folded into
	// the returned map for shapes that carry them.
	ToResource(uri, mimeType string) (map[string]any, error)

	// String returns the human-readable string form of the content (the text,
	// the raw data, the method, or the uri depending on type).
	String() string

	// SetMeta sets a single _meta key/value pair on the content. The value is
	// merged into any previously set metadata.
	SetMeta(key string, value any)

	// MergeMeta merges the given map into the content's _meta metadata.
	MergeMeta(meta map[string]any)
}

// Compile-time guarantees that every content type satisfies Content.
var (
	_ Content = (*Text)(nil)
	_ Content = (*Image)(nil)
	_ Content = (*Audio)(nil)
	_ Content = (*Blob)(nil)
	_ Content = (*ResourceLink)(nil)
	_ Content = (*Notification)(nil)
)

// meta is the embedded metadata helper shared by all content types: an optional
// _meta map merged into the wire shape under the "_meta" key when non-empty.
type meta struct {
	data map[string]any
}

// SetMeta sets a single metadata key/value pair.
func (m *meta) SetMeta(key string, value any) {
	if m.data == nil {
		m.data = make(map[string]any)
	}
	m.data[key] = value
}

// MergeMeta merges the given map into the metadata. A nil or empty map is a
// no-op.
func (m *meta) MergeMeta(meta map[string]any) {
	if len(meta) == 0 {
		return
	}
	if m.data == nil {
		m.data = make(map[string]any, len(meta))
	}
	for k, v := range meta {
		m.data[k] = v
	}
}

// merge returns base with the "_meta" key added when metadata is present.
// It does not mutate base in place beyond setting that single key.
func (m *meta) merge(base map[string]any) map[string]any {
	if len(m.data) == 0 {
		return base
	}
	if _, exists := base["_meta"]; !exists {
		base["_meta"] = m.cloneData()
	}
	return base
}

// cloneData returns a shallow copy of the metadata map so callers cannot mutate
// internal state through the marshaled output.
func (m *meta) cloneData() map[string]any {
	out := make(map[string]any, len(m.data))
	for k, v := range m.data {
		out[k] = v
	}
	return out
}
