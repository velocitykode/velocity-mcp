package content

import (
	"encoding/base64"
	"encoding/json"
)

// DefaultImageMimeType is the mimeType used when an Image is constructed
// without one.
const DefaultImageMimeType = "image/png"

// Image is binary image content. The constructor takes the raw (unencoded)
// image bytes; the wire shape base64-encodes them under "data".
// Wire shape: {"type":"image","data":"<base64>","mimeType":"..."}.
// In a resource context the encoded bytes are carried under "blob".
type Image struct {
	meta
	data     []byte
	mimeType string
}

// NewImage constructs an Image from raw bytes. When mimeType is empty,
// DefaultImageMimeType is used.
func NewImage(data []byte, mimeType string) *Image {
	if mimeType == "" {
		mimeType = DefaultImageMimeType
	}
	return &Image{data: data, mimeType: mimeType}
}

// String returns the raw image bytes as a string.
func (i *Image) String() string { return string(i.data) }

// encoded returns the base64-encoded data.
func (i *Image) encoded() string { return base64.StdEncoding.EncodeToString(i.data) }

// toArray returns the default tool/prompt wire shape.
func (i *Image) toArray() map[string]any {
	return i.merge(map[string]any{
		"type":     "image",
		"data":     i.encoded(),
		"mimeType": i.mimeType,
	})
}

// MarshalJSON encodes the default MCP wire shape.
func (i *Image) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.toArray())
}

// ToTool returns the tools/call result shape.
func (i *Image) ToTool() (map[string]any, error) { return i.toArray(), nil }

// ToPrompt returns the prompts/get message shape.
func (i *Image) ToPrompt() (map[string]any, error) { return i.toArray(), nil }

// ToResource returns the resources/read contents shape. The image's own
// mimeType is used; the resource's mimeType argument is ignored.
func (i *Image) ToResource(uri, mimeType string) (map[string]any, error) {
	return i.merge(map[string]any{
		"blob":     i.encoded(),
		"uri":      uri,
		"mimeType": i.mimeType,
	}), nil
}
