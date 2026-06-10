package mcptest

import (
	"encoding/json"
	"strings"
)

// asMaps coerces a decoded JSON value (expected to be an array of objects) into
// a slice of maps, skipping any element that is not an object. A nil or
// non-array value yields nil.
func asMaps(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, el := range arr {
		if m, ok := el.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// firstString returns the first string-valued entry among keys in m, or "".
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// containsAny reports whether sub is a substring of any element of in.
func containsAny(in []string, sub string) bool {
	for _, s := range in {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// dedupeNonEmpty returns the non-empty elements of in, de-duplicated, preserving
// first-seen order. It mirrors laravel/mcp's collect(...)->filter()->unique().
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// jsonEqual reports whether a and b are equal once normalised through JSON
// (so an int literal and the float64 a JSON decode produces compare equal).
func jsonEqual(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var an, bn any
	if json.Unmarshal(ab, &an) != nil || json.Unmarshal(bb, &bn) != nil {
		return false
	}
	xb, _ := json.Marshal(an)
	yb, _ := json.Marshal(bn)
	return string(xb) == string(yb)
}

// jsonUnmarshal is a thin alias so response.go does not import encoding/json
// directly (keeping the JSON dependency localised to this helpers file).
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
