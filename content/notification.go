package content

import "encoding/json"

// Notification is a JSON-RPC notification emitted as content. Unlike the other
// content types its wire shape is {"method":...,"params":{...}} and its _meta
// metadata is folded into params under "_meta" (not at the top level). It is
// valid in every context and the three context conversions all return the same
// shape.
type Notification struct {
	meta
	method string
	params map[string]any
}

// NewNotification constructs a Notification with the given method and params.
// A nil params map is treated as an empty object.
func NewNotification(method string, params map[string]any) *Notification {
	return &Notification{method: method, params: params}
}

// String returns the notification method.
func (n *Notification) String() string { return n.method }

// toArray returns the notification wire shape. Metadata, when present, is merged
// into the params object under "_meta" unless params already supplies that key.
func (n *Notification) toArray() map[string]any {
	params := make(map[string]any, len(n.params)+1)
	for k, v := range n.params {
		params[k] = v
	}

	if len(n.data) > 0 {
		if _, exists := params["_meta"]; !exists {
			params["_meta"] = n.cloneData()
		}
	}

	return map[string]any{
		"method": n.method,
		"params": params,
	}
}

// MarshalJSON encodes the notification wire shape.
func (n *Notification) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.toArray())
}

// ToTool returns the notification shape.
func (n *Notification) ToTool() (map[string]any, error) { return n.toArray(), nil }

// ToPrompt returns the notification shape.
func (n *Notification) ToPrompt() (map[string]any, error) { return n.toArray(), nil }

// ToResource returns the notification shape. The uri and mimeType arguments are
// ignored because a notification carries no resource binding.
func (n *Notification) ToResource(uri, mimeType string) (map[string]any, error) {
	return n.toArray(), nil
}
