package mcptest

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// This file reads the values a reply carries, for the text assertions. The
// result is decoded with its numbers kept as written, so a number is seen as
// the digits on the wire rather than as the float64 they round to.

// contentShapeMembers are the members of a content item that describe its
// shape rather than say anything: the item's kind, its media type, who it is
// addressed to, and its metadata. AssertText reads what a content item says,
// so these are left out of its view; AssertDontSeeText reads the whole result
// and leaves nothing out.
var contentShapeMembers = map[string]struct{}{
	"type": {}, "mimeType": {}, "annotations": {}, "_meta": {},
}

// decodeWireValue decodes raw as a generic JSON value whose numbers are kept as
// written. It reports false for bytes that do not hold one JSON value.
func decodeWireValue(raw json.RawMessage) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		// A second value follows the first: not one JSON value.
		return nil, false
	}
	return value, true
}

// scalarValues appends every scalar value under v, at any depth, rendered as
// text: a string as itself, a number as its wire digits, a boolean as its
// literal. Keys are structure rather than content and are not rendered; a
// member whose key skip names is left out along with everything under it. A
// null says nothing and is skipped.
func scalarValues(v any, skip map[string]struct{}, out []string) []string {
	switch value := v.(type) {
	case map[string]any:
		for key, member := range value {
			if _, skipped := skip[key]; skipped {
				continue
			}
			out = scalarValues(member, skip, out)
		}
	case []any:
		for _, element := range value {
			out = scalarValues(element, skip, out)
		}
	case string:
		out = append(out, value)
	case json.Number:
		out = append(out, value.String())
	case bool:
		out = append(out, strconv.FormatBool(value))
	}
	return out
}

// wireValues returns every scalar value the reply's result carries, wherever
// it sits: in a content item of any type, in the structured content, in the
// metadata, in a member that is not the shape the specification describes. It
// is the view AssertDontSeeText reads, since a text that must not be in the
// reply must not be anywhere in it. The values are returned sorted so a
// failure message is stable.
func (r *Response) wireValues() []string {
	if r.resp == nil || r.resp.Error != nil {
		return nil
	}
	value, ok := decodeWireValue(r.resp.Result)
	if !ok {
		return nil
	}
	return sortedUnique(scalarValues(value, nil, nil))
}

// sortedUnique returns the non-empty elements of in, de-duplicated and sorted.
func sortedUnique(in []string) []string {
	out := dedupeNonEmpty(in)
	sortStrings(out)
	return out
}

// quoteAll renders values for a failure message, each quoted so a value with
// spaces reads as one value.
func quoteAll(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return strings.Join(quoted, ", ")
}

// rawBool decodes a raw JSON boolean, reporting false for any other value.
func rawBool(raw json.RawMessage) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
