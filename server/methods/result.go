package methods

import (
	"github.com/velocitykode/velocity-mcp/server"
)

// promptResult builds the prompts/get result map from a response: a
// "description" and a "messages" array, each message carrying a role and a
// single content shape. A response with
// multiple content items yields multiple messages sharing the response role.
func promptResult(description string, resp *server.Response) (map[string]any, error) {
	messages := make([]any, 0)
	if resp != nil {
		for _, c := range resp.Contents() {
			shape, err := c.ToPrompt()
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{
				"role":    string(resp.Role()),
				"content": shape,
			})
		}
	}

	result := map[string]any{
		"description": description,
		"messages":    messages,
	}
	return mergeResponseMeta(resp, result), nil
}

// resourceResult builds the resources/read result map from a response: a
// "contents" array of per-item resource shapes carrying the resource uri and
// mimeType.
func resourceResult(uri, mimeType string, resp *server.Response) (map[string]any, error) {
	contents := make([]any, 0)
	if resp != nil {
		for _, c := range resp.Contents() {
			shape, err := c.ToResource(uri, mimeType)
			if err != nil {
				return nil, err
			}
			contents = append(contents, shape)
		}
	}

	result := map[string]any{
		"contents": contents,
	}
	return mergeResponseMeta(resp, result), nil
}

// mergeResponseMeta folds a response's _meta and structured content into the
// result map: keys are added only when present and never overwrite
// an existing key.
func mergeResponseMeta(resp *server.Response, result map[string]any) map[string]any {
	if resp == nil {
		return result
	}
	if meta := resp.Meta(); len(meta) > 0 {
		if _, exists := result["_meta"]; !exists {
			m := make(map[string]any, len(meta))
			for k, v := range meta {
				m[k] = v
			}
			result["_meta"] = m
		}
	}
	if structured := resp.StructuredContent(); len(structured) > 0 {
		if _, exists := result["structuredContent"]; !exists {
			result["structuredContent"] = structured
		}
	}
	return result
}
