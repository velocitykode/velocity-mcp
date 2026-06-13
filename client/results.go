package client

import (
	"encoding/base64"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// supportedProtocolVersions is the set of protocol versions this client accepts
// in an initialize result, mirroring the server's supported list.
var supportedProtocolVersions = map[string]bool{
	server.ProtocolV20251125: true,
	server.ProtocolV20250618: true,
	server.ProtocolV20250326: true,
	server.ProtocolV20241105: true,
}

// InitializeResult is the negotiated outcome of the initialize handshake.
type InitializeResult struct {
	ProtocolVersion string
	Capabilities    map[string]any
	ServerInfo      schema.Implementation
	Instructions    string
}

// parseInitializeResult decodes and validates an initialize result.
func parseInitializeResult(raw json.RawMessage) (*InitializeResult, error) {
	var payload struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Title       string `json:"title"`
			Description string `json:"description"`
			WebsiteURL  string `json:"websiteUrl"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid initialize response from server")
	}
	if !supportedProtocolVersions[payload.ProtocolVersion] || payload.ServerInfo.Name == "" || payload.ServerInfo.Version == "" {
		return nil, newError("invalid initialize response from server")
	}

	info := schema.NewImplementation(payload.ServerInfo.Name, payload.ServerInfo.Version)
	info.Title = payload.ServerInfo.Title
	info.Description = payload.ServerInfo.Description
	info.WebsiteURL = payload.ServerInfo.WebsiteURL

	return &InitializeResult{
		ProtocolVersion: payload.ProtocolVersion,
		Capabilities:    payload.Capabilities,
		ServerInfo:      info,
		Instructions:    payload.Instructions,
	}, nil
}

// ToolResult is the result of a tools/call request.
type ToolResult struct {
	Content           []map[string]any
	IsError           bool
	StructuredContent map[string]any
	Meta              map[string]any
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
		StructuredContent map[string]any   `json:"structuredContent"`
		Meta              map[string]any   `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, newError("invalid tools/call result from server")
	}
	return &ToolResult{
		Content:           payload.Content,
		IsError:           payload.IsError,
		StructuredContent: payload.StructuredContent,
		Meta:              payload.Meta,
	}, nil
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
