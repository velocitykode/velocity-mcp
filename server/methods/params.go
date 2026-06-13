package methods

import (
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// params is a decoded view over a JSON-RPC request's params object. It provides
// the small set of typed accessors the method handlers need (string, int, map).
type params map[string]any

// decode decodes a request's params into a params map. Absent or non-object
// params yield an empty map so accessors never panic.
func decode(req *jsonrpc.Request) params {
	if req == nil || len(req.Params) == 0 {
		return params{}
	}
	var m map[string]any
	if err := json.Unmarshal(req.Params, &m); err != nil || m == nil {
		return params{}
	}
	return params(m)
}

// str returns the string value for key, or "".
func (p params) str(key string) string {
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// has reports whether key is present.
func (p params) has(key string) bool {
	_, ok := p[key]
	return ok
}

// intValue returns the integer value for key (JSON numbers decode to float64),
// or 0 when absent or non-numeric.
func (p params) intValue(key string) int {
	switch v := p[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case int:
		return v
	}
	return 0
}

// intPtr returns a pointer to the integer value for key, or nil when the key is
// absent or non-numeric. It lets a caller distinguish an omitted parameter from
// an explicit 0 (JSON numbers decode to float64), which the page-size resolution
// needs to honor its null-coalescing default (an absent per_page
// falls back to the default; an explicit per_page of 0 does not).
func (p params) intPtr(key string) *int {
	v, ok := p[key]
	if !ok {
		return nil
	}
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return nil
		}
		n = int(i)
	case int:
		n = t
	default:
		return nil
	}
	return &n
}

// mapValue returns the map value for key, or nil.
func (p params) mapValue(key string) map[string]any {
	if v, ok := p[key].(map[string]any); ok {
		return v
	}
	return nil
}

// arguments returns the "arguments" object as a map, or an empty map. This is
// the tool/prompt argument bag the primitive handlers receive.
func (p params) arguments() map[string]any {
	if v, ok := p["arguments"].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}
