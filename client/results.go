package client

import (
	"encoding/base64"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/schema"
)

// InitializeResult is the negotiated outcome of the initialize handshake.
type InitializeResult struct {
	ProtocolVersion string
	Capabilities    map[string]any
	ServerInfo      schema.Implementation
	Instructions    string
}

// parseInitializeResult decodes and validates an initialize result. The version
// the server chose has to be one this client speaks through the initialize
// handshake: a client that carried on regardless would be sending a wire shape
// neither peer agreed on. A version that is not a string settled on nothing, so
// it is reported as none rather than as a malformed payload.
func parseInitializeResult(raw json.RawMessage) (*InitializeResult, error) {
	var payload struct {
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
		Instructions    json.RawMessage `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid initialize response from server")
	}

	var chosen string
	_ = decodeMember(payload.ProtocolVersion, &chosen)
	if !supportsInitializeVersion(chosen) {
		reported := chosen
		if reported == "" {
			reported = "none"
		}
		return nil, newError("the server chose protocol version [" + reported +
			"]; this client supports [" + joinVersions(initializeSupportedVersions()) + "]")
	}

	var capabilities map[string]any
	if err := decodeMember(payload.Capabilities, &capabilities); err != nil || capabilities == nil {
		return nil, newError("invalid initialize response from server")
	}
	info, ok := parseImplementation(payload.ServerInfo)
	if !ok {
		return nil, newError("invalid initialize response from server")
	}

	// Instructions are optional: a payload that carries them in another shape is
	// read as if it carried none.
	result := &InitializeResult{
		ProtocolVersion: chosen,
		Capabilities:    capabilities,
		ServerInfo:      info,
	}
	_ = decodeMember(payload.Instructions, &result.Instructions)
	return result, nil
}

// ToolResult is the result of a tools/call request.
type ToolResult struct {
	Content []map[string]any
	IsError bool
	// StructuredContent is the machine-readable result the tool returned,
	// decoded as whatever JSON value it is: an object, an array, a string, a
	// number, or a boolean. A tool's outputSchema may describe any of them, so
	// reading it as an object alone would refuse results the protocol permits.
	// It is nil when the result carries none and when it carries an explicit
	// null; HasStructuredContent tells the two apart.
	StructuredContent any
	// HasStructuredContent reports whether the result carried the member at all.
	HasStructuredContent bool
	Meta                 map[string]any
}

// StructuredObject returns the structured content as an object, reporting false
// when the tool returned another JSON type or none at all. It is the common
// case of StructuredContent, spelled so a caller need not assert the type.
func (r ToolResult) StructuredObject() (map[string]any, bool) {
	object, ok := r.StructuredContent.(map[string]any)
	return object, ok
}

// Text concatenates the text of every text content block, ignoring other types.
func (r ToolResult) Text() string {
	return joinTextBlocks(r.Content)
}

// parseToolResult decodes a tools/call result.
func parseToolResult(raw json.RawMessage) (*ToolResult, error) {
	var payload struct {
		Content           []map[string]any `json:"content"`
		IsError           bool             `json:"isError"`
		StructuredContent json.RawMessage  `json:"structuredContent"`
		Meta              map[string]any   `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid tools/call result from server")
	}
	result := &ToolResult{
		Content: payload.Content,
		IsError: payload.IsError,
		Meta:    payload.Meta,
	}
	if len(payload.StructuredContent) > 0 {
		if err := json.Unmarshal(payload.StructuredContent, &result.StructuredContent); err != nil {
			return nil, newError("invalid tools/call result from server")
		}
		result.HasStructuredContent = true
	}
	return result, nil
}

// ResourceReadResult is the result of a resources/read request.
type ResourceReadResult struct {
	Contents []map[string]any
	Meta     map[string]any
}

// parseResourceReadResult decodes a resources/read result.
func parseResourceReadResult(raw json.RawMessage) (*ResourceReadResult, error) {
	var payload struct {
		Contents []map[string]any `json:"contents"`
		Meta     map[string]any   `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid resources/read result from server")
	}
	return &ResourceReadResult{Contents: payload.Contents, Meta: payload.Meta}, nil
}

// MimeType returns the first non-empty mimeType across the contents, or "".
func (r ResourceReadResult) MimeType() string {
	for _, c := range r.Contents {
		if mt, ok := c["mimeType"].(string); ok && mt != "" {
			return mt
		}
	}
	return ""
}

// Content concatenates every content block, decoding base64 blobs to their raw
// bytes and appending text blocks verbatim.
func (r ResourceReadResult) Content() string {
	var b []byte
	for _, c := range r.Contents {
		if text, ok := c["text"].(string); ok {
			b = append(b, text...)
			continue
		}
		if blob, ok := c["blob"].(string); ok {
			if decoded, err := base64.StdEncoding.DecodeString(blob); err == nil {
				b = append(b, decoded...)
			}
		}
	}
	return string(b)
}

// PromptResult is the result of a prompts/get request.
type PromptResult struct {
	Messages    []map[string]any
	Description string
	Meta        map[string]any
}

// parsePromptResult decodes a prompts/get result.
func parsePromptResult(raw json.RawMessage) (*PromptResult, error) {
	var payload struct {
		Messages    []map[string]any `json:"messages"`
		Description string           `json:"description"`
		Meta        map[string]any   `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid prompts/get result from server")
	}
	return &PromptResult{Messages: payload.Messages, Description: payload.Description, Meta: payload.Meta}, nil
}

// Text concatenates the text of every text-typed message content block.
func (r PromptResult) Text() string {
	var out string
	for _, m := range r.Messages {
		content, ok := m["content"].(map[string]any)
		if !ok {
			continue
		}
		if content["type"] == "text" {
			if text, ok := content["text"].(string); ok {
				out += text
			}
		}
	}
	return out
}

// joinTextBlocks concatenates the text of text-typed content blocks.
func joinTextBlocks(blocks []map[string]any) string {
	var out string
	for _, b := range blocks {
		if b["type"] == "text" {
			if text, ok := b["text"].(string); ok {
				out += text
			}
		}
	}
	return out
}
