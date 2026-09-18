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
// first-seen order.
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

// jsonEqual reports whether a and b are the same JSON document once both are
// serialized (so an int literal and the float64 a JSON decode produces compare
// equal). Either side may be a json.RawMessage, in which case the bytes the
// server wrote are what is compared: see comparableJSON in jsonvalue.go for how
// numbers are judged.
func jsonEqual(a, b any) bool {
	an, aok := comparableJSON(a)
	bn, bok := comparableJSON(b)
	return aok && bok && an == bn
}

// jsonUnmarshal is a thin alias so response.go does not import encoding/json
// directly (keeping the JSON dependency localised to this helpers file).
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// jsonString renders a value as compact JSON for a failure message. A value that
// cannot be encoded (never produced by a decoded reply, but possible for a
// caller-supplied expectation) renders as a fixed marker so an assertion still
// reports something deterministic instead of panicking.
func jsonString(v any) string {
	b, err := encodeJSON(v)
	if err != nil {
		return "<unencodable value>"
	}
	return string(b)
}
