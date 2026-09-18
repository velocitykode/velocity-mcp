package mcptest

import "encoding/json"

// This file adds the structuredContent assertions. A tools/call result may carry
// a machine-readable "structuredContent" object beside the human-readable
// content items (MCP structured tool output). An expectation is serialized and
// compared against the bytes the server wrote, so a Go int, a typed struct and
// the object on the wire are judged as the same document, while a number that
// differs beyond float64's precision still fails.

// StructuredContent returns the reply's structuredContent object, or nil when
// the reply carries none (or is an error reply). It is the decoded view, so its
// numbers are float64; the assertions below compare the wire bytes instead. The
// map is the reply's own decoded view, not a copy: read it, do not mutate it, or
// the assertions that follow will read what the mutation left behind.
func (r *Response) StructuredContent() map[string]any {
	if r.result == nil {
		return nil
	}
	obj, ok := r.result["structuredContent"].(map[string]any)
	if !ok {
		return nil
	}
	return obj
}

// structuredRaw returns the structuredContent value as the server wrote it,
// reporting false only when the reply carries no such member. Whether the member
// is an object is a separate question from whether it is there: a tool's
// outputSchema may describe an array, a string, or any other JSON value, and an
// explicit null is a value the tool returned rather than a value it omitted.
func (r *Response) structuredRaw() (json.RawMessage, bool) {
	return r.rawResultPath("structuredContent")
}

// AssertStructuredContent asserts the reply's structuredContent equals want,
// whole: every key the reply carries must be in want and vice versa. want may be
// any JSON-encodable value (a map, a typed struct, values that serialize to
// numbers or strings); it is serialized before the comparison, so it may be the
// very type the tool returns.
func (r *Response) AssertStructuredContent(want any) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	got, ok := r.structuredRaw()
	if !ok {
		r.fatalf("mcptest: %s: expected structured content %s, but the reply carries none",
			r.method, jsonString(want))
		return r
	}
	if !jsonEqual(got, want) {
		r.fatalf("mcptest: %s: structured content = %s, want %s",
			r.method, describeJSON(got), jsonString(want))
	}
	return r
}

// AssertStructuredContentKey asserts the reply's structuredContent binds key to
// want (compared as their serialized forms). It is the targeted form of
// AssertStructuredContent for a reply whose other keys are not under test.
func (r *Response) AssertStructuredContentKey(key string, want any) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	got, ok := r.structuredRaw()
	if !ok {
		r.fatalf("mcptest: %s: expected structured content with key %q, but the reply carries none",
			r.method, key)
		return r
	}
	fields, isObject := rawObject(got)
	if !isObject {
		// This is the one assertion that needs an object: a key cannot be read
		// out of an array or a scalar, and saying the key is missing would
		// describe the wrong fault.
		r.fatalf("mcptest: %s: expected structured content with key %q, but it is not an object: %s",
			r.method, key, describeJSON(got))
		return r
	}
	value, present := fields[key]
	if !present {
		r.fatalf("mcptest: %s: structured content is missing key %q; it has: %s",
			r.method, key, describeJSON(got))
		return r
	}
	if !jsonEqual(value, want) {
		r.fatalf("mcptest: %s: structuredContent[%q] = %s, want %s",
			r.method, key, describeJSON(value), jsonString(want))
	}
	return r
}

// AssertNoStructuredContent asserts the reply carries no structuredContent
// member at all, which is the expected shape for a tool that returns only
// human-readable content. A tool that returned an explicit null did carry one,
// so it fails here: a null result and no result are different answers.
func (r *Response) AssertNoStructuredContent() *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if got, ok := r.structuredRaw(); ok {
		r.fatalf("mcptest: %s: expected no structured content, got %s", r.method, describeJSON(got))
	}
	return r
}
