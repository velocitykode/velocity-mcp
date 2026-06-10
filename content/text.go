package content

import "encoding/json"

// Text is plain text content. Wire shape: {"type":"text","text":"..."}.
// In a resource context it carries the owning resource's uri and mimeType
// instead of a "type" discriminator. Mirrors laravel/mcp Server\Content\Text.
type Text struct {
	meta
	text string
}

// NewText constructs a Text content value.
func NewText(text string) *Text {
	return &Text{text: text}
}

// String returns the underlying text.
func (t *Text) String() string { return t.text }

// toArray returns the default tool/prompt wire shape.
func (t *Text) toArray() map[string]any {
	return t.merge(map[string]any{
		"type": "text",
		"text": t.text,
	})
}

// MarshalJSON encodes the default MCP wire shape.
func (t *Text) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.toArray())
}

// ToTool returns the tools/call result shape.
func (t *Text) ToTool() (map[string]any, error) { return t.toArray(), nil }

// ToPrompt returns the prompts/get message shape.
func (t *Text) ToPrompt() (map[string]any, error) { return t.toArray(), nil }

// ToResource returns the resources/read contents shape, carrying the owning
// resource's uri and mimeType.
func (t *Text) ToResource(uri, mimeType string) (map[string]any, error) {
	return t.merge(map[string]any{
		"text":     t.text,
		"uri":      uri,
		"mimeType": mimeType,
	}), nil
}
