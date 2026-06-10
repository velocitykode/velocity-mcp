package content

import (
	"encoding/base64"
	"encoding/json"
)

// Blob is embedded binary resource content. It may only be used in a resource
// (resources/read) context; using it as a tool or prompt result returns
// ErrNotAllowed, mirroring laravel/mcp which throws there.
//
// The default wire shape (toArray) is {"type":"blob","blob":"<raw content>"}
// and carries the content verbatim (no base64), matching laravel/mcp. The
// resource shape base64-encodes the content under "blob" alongside uri and
// mimeType. Mirrors laravel/mcp Server\Content\Blob.
type Blob struct {
	meta
	content []byte
}

// NewBlob constructs a Blob from raw bytes.
func NewBlob(content []byte) *Blob {
	return &Blob{content: content}
}

// String returns the raw blob bytes as a string.
func (b *Blob) String() string { return string(b.content) }

// toArray returns the default blob wire shape. Per laravel/mcp the content is
// carried verbatim here (not base64-encoded).
func (b *Blob) toArray() map[string]any {
	return b.merge(map[string]any{
		"type": "blob",
		"blob": string(b.content),
	})
}

// MarshalJSON encodes the default MCP wire shape.
func (b *Blob) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.toArray())
}

// ToTool returns ErrNotAllowed: blob content may not be used in tools.
func (b *Blob) ToTool() (map[string]any, error) { return nil, ErrNotAllowed }

// ToPrompt returns ErrNotAllowed: blob content may not be used in prompts.
func (b *Blob) ToPrompt() (map[string]any, error) { return nil, ErrNotAllowed }

// ToResource returns the resources/read contents shape with the content
// base64-encoded under "blob".
func (b *Blob) ToResource(uri, mimeType string) (map[string]any, error) {
	return b.merge(map[string]any{
		"blob":     base64.StdEncoding.EncodeToString(b.content),
		"uri":      uri,
		"mimeType": mimeType,
	}), nil
}
