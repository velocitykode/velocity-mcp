package content

import (
	"encoding/base64"
	"encoding/json"
)

// DefaultAudioMimeType is the mimeType used when an Audio is constructed
// without one.
const DefaultAudioMimeType = "audio/wav"

// Audio is binary audio content. The constructor takes the raw (unencoded)
// audio bytes; the wire shape base64-encodes them under "data".
// Wire shape: {"type":"audio","data":"<base64>","mimeType":"..."}.
// In a resource context the encoded bytes are carried under "blob".
type Audio struct {
	meta
	data     []byte
	mimeType string
}

// NewAudio constructs an Audio from raw bytes. When mimeType is empty,
// DefaultAudioMimeType is used.
func NewAudio(data []byte, mimeType string) *Audio {
	if mimeType == "" {
		mimeType = DefaultAudioMimeType
	}
	return &Audio{data: data, mimeType: mimeType}
}

// String returns the raw audio bytes as a string.
func (a *Audio) String() string { return string(a.data) }

// encoded returns the base64-encoded data.
func (a *Audio) encoded() string { return base64.StdEncoding.EncodeToString(a.data) }

// toArray returns the default tool/prompt wire shape.
func (a *Audio) toArray() map[string]any {
	return a.merge(map[string]any{
		"type":     "audio",
		"data":     a.encoded(),
		"mimeType": a.mimeType,
	})
}

// MarshalJSON encodes the default MCP wire shape.
func (a *Audio) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.toArray())
}

// ToTool returns the tools/call result shape.
func (a *Audio) ToTool() (map[string]any, error) { return a.toArray(), nil }

// ToPrompt returns the prompts/get message shape.
func (a *Audio) ToPrompt() (map[string]any, error) { return a.toArray(), nil }

// ToResource returns the resources/read contents shape. The audio's own
// mimeType is used; the resource's mimeType argument is ignored.
func (a *Audio) ToResource(uri, mimeType string) (map[string]any, error) {
	return a.merge(map[string]any{
		"blob":     a.encoded(),
		"uri":      uri,
		"mimeType": a.mimeType,
	}), nil
}
