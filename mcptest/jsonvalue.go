package mcptest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file holds the JSON plumbing the value assertions compare against: the
// raw bytes of a field as the server wrote it, a canonical rendering used for
// failure messages, and the comparable form equality runs on.
//
// Both forms start from the wire bytes rather than from a decoded map because
// decoding a JSON number into an interface yields a float64, which silently
// rounds an integer above 2^53 (record ids in structured tool output routinely
// exceed it). Decoding in number mode keeps every number in the exact form it
// was written, so an assertion on such an id can fail.
//
// The two forms differ in one point, numbers. A failure message must show what
// the server actually wrote, so the rendering keeps a number's literal.
// Equality must not depend on spelling, so the comparable form rewrites each
// number into one canonical spelling: 10, 10.0 and 1e1 are the same value,
// while 9007199254740993 and 9007199254740992 stay apart.

// rawFields is a JSON object whose members are kept in their wire form.
type rawFields = map[string]json.RawMessage

// encodeJSON renders v as compact JSON with no HTML escaping, so a string
// carrying <, > or & reaches a failure message as the server wrote it rather
// than as < escapes. Equality is unaffected: both sides are rendered by
// this one function.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode terminates the value with a newline; the callers want the value
	// alone.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// decodeJSONValue decodes v into a tree that keeps every number as its literal.
// A json.RawMessage is decoded as it stands; any other value is serialized
// first, so a Go int, a typed struct and the bytes a server sent all reduce to
// one comparable tree. It reports false for a value that cannot be serialized
// and for bytes that do not hold exactly one JSON value.
func decodeJSONValue(v any) (any, bool) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		encoded, err := encodeJSON(v)
		if err != nil {
			return nil, false
		}
		raw = encoded
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, false
	}
	// Decoding again is what settles whether the value stood alone. Asking the
	// decoder whether there is More would not do: it answers no for a trailing
	// "]" or "}", the tokens that end a value it is not inside, so bytes such as
	// `{"a":1}]` would compare equal to `{"a":1}` and an assertion could hold
	// against an expectation that is not valid JSON at all.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return decoded, true
}

// canonicalJSON renders v as canonical JSON: object keys sorted, insignificant
// whitespace dropped, and numbers kept verbatim. It is the rendering used in
// failure messages; comparableJSON is what equality runs on. It reports false
// for a value that cannot be serialized and for bytes that do not hold exactly
// one JSON value.
func canonicalJSON(v any) (string, bool) {
	decoded, ok := decodeJSONValue(v)
	if !ok {
		return "", false
	}
	out, err := encodeJSON(decoded)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// comparableJSON renders v in the form equality runs on: canonicalJSON's shape
// with every number rewritten to one spelling, so a wire value of 10.0 and an
// expectation of the Go int 10 are the same document while an id beyond
// float64's exact range still differs from its neighbour.
func comparableJSON(v any) (string, bool) {
	decoded, ok := decodeJSONValue(v)
	if !ok {
		return "", false
	}
	out, err := encodeJSON(normaliseNumbers(decoded))
	if err != nil {
		return "", false
	}
	return string(out), true
}

// normaliseNumbers rewrites every number in a decoded JSON tree (in place, the
// tree being freshly decoded for this comparison) into its canonical spelling.
func normaliseNumbers(v any) any {
	switch value := v.(type) {
	case map[string]any:
		for k, member := range value {
			value[k] = normaliseNumbers(member)
		}
		return value
	case []any:
		for i, element := range value {
			value[i] = normaliseNumbers(element)
		}
		return value
	case json.Number:
		return json.Number(canonicalNumber(string(value)))
	default:
		return v
	}
}

// canonicalNumber rewrites a JSON number literal into one spelling: the
// significant digits with leading and trailing zeros removed, times a power of
// ten (10, 10.0 and 1e1 all become 1e1). The digits are never expanded, so
// 9007199254740993 keeps every one of them and still differs from its
// neighbour, and an exponent no reply could carry costs nothing to handle. A
// literal this cannot read (or whose exponent does not fit in an int) is
// returned as written, which is exact for every literal that can reach it.
func canonicalNumber(literal string) string {
	digits := literal

	sign := ""
	if rest, found := strings.CutPrefix(digits, "-"); found {
		sign, digits = "-", rest
	}

	exponent := 0
	if i := strings.IndexAny(digits, "eE"); i >= 0 {
		parsed, err := strconv.Atoi(digits[i+1:])
		if err != nil {
			return literal
		}
		// Bound the exponent well inside an int so the adjustments below cannot
		// overflow on any platform. No reply carries a number near this.
		if parsed > 1<<30 || parsed < -(1<<30) {
			return literal
		}
		exponent = parsed
		digits = digits[:i]
	}

	if i := strings.IndexByte(digits, '.'); i >= 0 {
		fraction := digits[i+1:]
		digits = digits[:i] + fraction
		exponent -= len(fraction)
	}

	digits = strings.TrimLeft(digits, "0")
	for strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		exponent++
	}
	if digits == "" {
		// Every spelling of zero (0, -0, 0.000, 0e12) is one value.
		return "0"
	}
	return sign + digits + "e" + strconv.Itoa(exponent)
}

// describeJSON renders a raw JSON value for a failure message: its canonical
// form, the bytes as they arrived when they do not hold one JSON value, and an
// explicit marker when there are none at all.
func describeJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(none)"
	}
	if out, ok := canonicalJSON(raw); ok {
		return out
	}
	return string(raw)
}

// rawObject decodes a raw JSON value as an object whose members stay in their
// wire form, reporting false for null, an array, a scalar, or malformed bytes.
func rawObject(raw json.RawMessage) (rawFields, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var fields rawFields
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// rawString decodes a raw JSON string, reporting false for any other type. The
// token is inspected before decoding because encoding/json reads a JSON null
// into a string without complaint and would report it as an empty string.
func rawString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

// rawItems decodes a raw JSON array into its elements, keeping each in its wire
// form. A value that is not an array yields nil.
func rawItems(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	return items
}

// joinRawArray renders elements as one JSON array, which is how the list
// drivers rebuild a catalogue merged from several pages.
func joinRawArray(elements []json.RawMessage) json.RawMessage {
	out := make([]byte, 0, len(elements)*32+2)
	out = append(out, '[')
	for i, element := range elements {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, element...)
	}
	return append(out, ']')
}

// decodeResultFields decodes a success reply's result object into its raw
// members, or nil for an error reply, a notification (nil resp), or a result
// that is not an object.
func decodeResultFields(resp *jsonrpc.Response) rawFields {
	if resp == nil || resp.Error != nil || len(resp.Result) == 0 {
		return nil
	}
	fields, _ := rawObject(json.RawMessage(resp.Result))
	return fields
}

// rawResultPath returns the wire bytes of a value inside the reply's result
// object, walking keys from the top. It reports false when the reply carries no
// result object or the path does not resolve to a value.
func (r *Response) rawResultPath(keys ...string) (json.RawMessage, bool) {
	fields := r.raw
	for i, key := range keys {
		if fields == nil {
			return nil, false
		}
		value, ok := fields[key]
		if !ok {
			return nil, false
		}
		if i == len(keys)-1 {
			return value, true
		}
		fields, _ = rawObject(value)
	}
	return nil, false
}
