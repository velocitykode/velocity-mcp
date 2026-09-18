package mcptest

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file drives the assertions against replies a conforming server never
// sends. They matter because an assertion that holds against a malformed reply
// certifies nothing: a count that silently drops what it cannot read, or a
// "carries none" that is really "carries something I did not expect", turns a
// broken server into a green test.

// assertFails runs an assertion against a recording harness and reports the
// message it failed with, failing the test when it held instead.
func assertFails(t *testing.T, r *Response, assert func(*Response)) string {
	t.Helper()
	tb := &recordingTB{}
	r.t = tb
	assert(r)
	if len(tb.messages) != 1 {
		t.Fatalf("expected exactly one failure, got %d: %v", len(tb.messages), tb.messages)
	}
	return tb.last()
}

// assertHolds runs an assertion that must not fail.
func assertHolds(t *testing.T, r *Response, assert func(*Response)) {
	t.Helper()
	tb := &recordingTB{}
	r.t = tb
	assert(r)
	if len(tb.messages) != 0 {
		t.Fatalf("expected the assertion to hold, it failed with: %v", tb.messages)
	}
}

// TestCompletionAssertionsRefuseMalformedCandidates asserts a candidate list the
// specification does not permit fails every assertion that reads it, rather than
// being counted as the entries that could be read out of it.
func TestCompletionAssertionsRefuseMalformedCandidates(t *testing.T) {
	tests := []struct {
		name       string
		completion string
		wantReason string
	}{
		{
			name:       "no values member at all",
			completion: `{"total":0}`,
			wantReason: "carries no [values] member",
		},
		{
			name:       "an empty completion object",
			completion: `{}`,
			wantReason: "carries no [values] member",
		},
		{
			name:       "values stated as null",
			completion: `{"values":null}`,
			wantReason: "[values] member is not an array",
		},
		{
			name:       "values stated as a string",
			completion: `{"values":"go"}`,
			wantReason: "[values] member is not an array",
		},
		{
			name:       "values stated as an object",
			completion: `{"values":{"0":"go"}}`,
			wantReason: "[values] member is not an array",
		},
		{
			name:       "a numeric candidate",
			completion: `{"values":["go",42]}`,
			wantReason: "not a string at index 1",
		},
		{
			name:       "a null candidate",
			completion: `{"values":[null]}`,
			wantReason: "not a string at index 0",
		},
		{
			name:       "an object candidate",
			completion: `{"values":[{"value":"go"}]}`,
			wantReason: "not a string at index 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":1,"result":{"completion":` + tc.completion + `}}`

			for _, assertion := range []struct {
				name   string
				assert func(*Response)
			}{
				{"AssertCompletionCount(0)", func(r *Response) { r.AssertCompletionCount(0) }},
				{"AssertCompletionCount(1)", func(r *Response) { r.AssertCompletionCount(1) }},
				{"AssertCompletionValues()", func(r *Response) { r.AssertCompletionValues() }},
				{"AssertHasCompletions()", func(r *Response) { r.AssertHasCompletions() }},
			} {
				r := replyFrom(t, nil, "completion/complete", frame)
				message := assertFails(t, r, assertion.assert)
				if !strings.Contains(message, tc.wantReason) {
					t.Fatalf("%s failed with %q, want it to name %q", assertion.name, message, tc.wantReason)
				}
			}
		})
	}
}

// TestCompletionAssertionsHoldForWellFormedCandidates asserts the new strictness
// refuses nothing a server may legitimately send, including an empty candidate
// list and candidates carrying unicode or characters a naive reader might drop.
func TestCompletionAssertionsHoldForWellFormedCandidates(t *testing.T) {
	t.Run("an empty candidate list", func(t *testing.T) {
		r := replyFrom(t, nil, "completion/complete",
			`{"jsonrpc":"2.0","id":1,"result":{"completion":{"values":[],"total":0}}}`)
		assertHolds(t, r, func(r *Response) { r.AssertCompletionCount(0) })
		assertHolds(t, r, func(r *Response) { r.AssertCompletionValues() })
	})

	t.Run("candidates carrying unicode and an empty string", func(t *testing.T) {
		r := replyFrom(t, nil, "completion/complete",
			`{"jsonrpc":"2.0","id":1,"result":{"completion":{"values":["café","","a\tb"]}}}`)
		assertHolds(t, r, func(r *Response) { r.AssertCompletionCount(3) })
		assertHolds(t, r, func(r *Response) { r.AssertCompletionValues("café", "", "a\tb") })
		assertHolds(t, r, func(r *Response) { r.AssertHasCompletions("café") })
	})
}

// TestRegistrationCountsRefuseMalformedEntries asserts a catalogue holding
// something that is not a primitive fails the registration assertions instead of
// being counted as though those entries were not there. A "not listed" that
// holds against such a page reports a registered primitive as absent.
func TestRegistrationCountsRefuseMalformedEntries(t *testing.T) {
	tests := []struct {
		name  string
		list  string
		index string
	}{
		{name: "numbers and nulls", list: `[42,null]`, index: "index 0"},
		{name: "a stray null after a real entry", list: `[{"name":"doc"},null]`, index: "index 1"},
		{name: "a nested array", list: `[["doc"]]`, index: "index 0"},
		{name: "a bare string", list: `["doc"]`, index: "index 0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":1,"result":{"resources":` + tc.list + `}}`

			for _, assertion := range []struct {
				name   string
				assert func(*Response)
			}{
				{"AssertResourceCount(0)", func(r *Response) { r.AssertResourceCount(0) }},
				{"AssertResourceCount(2)", func(r *Response) { r.AssertResourceCount(2) }},
				{"AssertResourceNotListed", func(r *Response) { r.AssertResourceNotListed("doc") }},
				{"AssertResourceListed", func(r *Response) { r.Resource("doc") }},
			} {
				r := replyFrom(t, nil, "resources/list", frame)
				message := assertFails(t, r, assertion.assert)
				if !strings.Contains(message, "is not an object") || !strings.Contains(message, tc.index) {
					t.Fatalf("%s failed with %q, want it to name the entry at %s", assertion.name, message, tc.index)
				}
			}
		})
	}
}

// namedCatalogue is one of the four list methods whose entries the registration
// assertions match by name, with the assertions that read that catalogue.
type namedCatalogue struct {
	method     string
	key        string
	assertions []struct {
		name   string
		assert func(*Response)
	}
}

// namedCatalogues enumerates the four list methods and the registration
// assertions over each, so a malformed-entry case can be driven against every
// catalogue rather than one standing in for the rest.
func namedCatalogues() []namedCatalogue {
	type assertion = struct {
		name   string
		assert func(*Response)
	}
	return []namedCatalogue{
		{method: "tools/list", key: "tools", assertions: []assertion{
			{"AssertToolCount(1)", func(r *Response) { r.AssertToolCount(1) }},
			{"AssertToolNotListed", func(r *Response) { r.AssertToolNotListed("doc") }},
			{"AssertToolListed", func(r *Response) { r.AssertToolListed("doc") }},
			{"Tool", func(r *Response) { r.Tool("doc") }},
		}},
		{method: "resources/list", key: "resources", assertions: []assertion{
			{"AssertResourceCount(1)", func(r *Response) { r.AssertResourceCount(1) }},
			{"AssertResourceNotListed", func(r *Response) { r.AssertResourceNotListed("doc") }},
			{"AssertResourceListed", func(r *Response) { r.AssertResourceListed("doc") }},
			{"Resource", func(r *Response) { r.Resource("doc") }},
		}},
		{method: "resources/templates/list", key: "resourceTemplates", assertions: []assertion{
			{"AssertResourceTemplateCount(1)", func(r *Response) { r.AssertResourceTemplateCount(1) }},
			{"AssertResourceTemplateNotListed", func(r *Response) { r.AssertResourceTemplateNotListed("doc") }},
			{"AssertResourceTemplateListed", func(r *Response) { r.AssertResourceTemplateListed("doc") }},
			{"ResourceTemplate", func(r *Response) { r.ResourceTemplate("doc") }},
		}},
		{method: "prompts/list", key: "prompts", assertions: []assertion{
			{"AssertPromptCount(1)", func(r *Response) { r.AssertPromptCount(1) }},
			{"AssertPromptNotListed", func(r *Response) { r.AssertPromptNotListed("doc") }},
			{"AssertPromptListed", func(r *Response) { r.AssertPromptListed("doc") }},
			{"Prompt", func(r *Response) { r.Prompt("doc") }},
		}},
	}
}

// TestRegistrationAssertionsRefuseNamelessEntries asserts an entry carrying no
// string name fails the registration assertions instead of being dropped from
// the catalogue. The specification makes "name" a required member of a tool, a
// resource, a resource template, and a prompt alike, and every registration
// assertion matches on it, so an entry without one is a primitive those
// assertions are blind to: a reply of {"tools":[{}]} would otherwise satisfy
// AssertToolNotListed for every name there is, and report a registered tool as
// absent.
func TestRegistrationAssertionsRefuseNamelessEntries(t *testing.T) {
	tests := []struct {
		name  string
		list  string
		index string
	}{
		{name: "an entry with no members", list: `[{}]`, index: "index 0"},
		{name: "a numeric name", list: `[{"name":42}]`, index: "index 0"},
		{name: "a null name", list: `[{"name":null}]`, index: "index 0"},
		{name: "an object name", list: `[{"name":{"value":"doc"}}]`, index: "index 0"},
		{name: "an array name", list: `[{"name":["doc"]}]`, index: "index 0"},
		{name: "a titled entry after a real one", list: `[{"name":"doc"},{"title":"notes"}]`, index: "index 1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, catalogue := range namedCatalogues() {
				frame := `{"jsonrpc":"2.0","id":1,"result":{"` + catalogue.key + `":` + tc.list + `}}`
				for _, assertion := range catalogue.assertions {
					r := replyFrom(t, nil, catalogue.method, frame)
					message := assertFails(t, r, assertion.assert)
					if !strings.Contains(message, `carries no string "name"`) || !strings.Contains(message, tc.index) {
						t.Fatalf("%s %s failed with %q, want it to name the entry at %s",
							catalogue.method, assertion.name, message, tc.index)
					}
				}
			}
		})
	}
}

// TestRegistrationAssertionsHoldForNamedEntries asserts the name check refuses
// nothing a server may legitimately send: an entry carrying members beyond its
// name, and one whose name is the empty string, which is a string like any
// other.
func TestRegistrationAssertionsHoldForNamedEntries(t *testing.T) {
	tests := []struct {
		name string
		list string
	}{
		{name: "a name beside other members", list: `[{"name":"doc","title":"Doc","description":"d"}]`},
		{name: "an empty name", list: `[{"name":""}]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, catalogue := range namedCatalogues() {
				frame := `{"jsonrpc":"2.0","id":1,"result":{"` + catalogue.key + `":` + tc.list + `}}`
				// The count is the one assertion that holds for either entry; the
				// rest look for the name "doc", which only the first carries.
				r := replyFrom(t, nil, catalogue.method, frame)
				assertHolds(t, r, catalogue.assertions[0].assert)
			}
		})
	}
}

// TestRegistrationCountsHoldForWellFormedLists asserts the shape check leaves a
// proper catalogue alone, including an empty one.
func TestRegistrationCountsHoldForWellFormedLists(t *testing.T) {
	t.Run("an empty catalogue", func(t *testing.T) {
		r := replyFrom(t, nil, "resources/list", `{"jsonrpc":"2.0","id":1,"result":{"resources":[]}}`)
		assertHolds(t, r, func(r *Response) { r.AssertResourceCount(0) })
		assertHolds(t, r, func(r *Response) { r.AssertResourceNotListed("doc") })
	})

	t.Run("a catalogue of objects", func(t *testing.T) {
		r := replyFrom(t, nil, "resources/list",
			`{"jsonrpc":"2.0","id":1,"result":{"resources":[{"name":"doc"},{"name":"notes"}]}}`)
		assertHolds(t, r, func(r *Response) { r.AssertResourceCount(2) })
		assertHolds(t, r, func(r *Response) { r.AssertResourceNotListed("missing") })
	})
}

// TestStructuredContentAssertionsSeePresenceNotShape asserts the presence of the
// member is judged apart from its JSON type: a tool whose outputSchema describes
// an array or a scalar returns one, and a result carrying an explicit null
// carries a value rather than none.
func TestStructuredContentAssertionsSeePresenceNotShape(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  any
	}{
		{name: "an array", value: `[1,2]`, want: []int{1, 2}},
		{name: "an array of objects", value: `[{"id":1}]`, want: []map[string]int{{"id": 1}}},
		{name: "a string", value: `"ok"`, want: "ok"},
		{name: "a number", value: `42`, want: 42},
		{name: "a boolean", value: `true`, want: true},
		{name: "an explicit null", value: `null`, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":` + tc.value + `}}`

			r := replyFrom(t, nil, "tools/call", frame)
			assertHolds(t, r, func(r *Response) { r.AssertStructuredContent(tc.want) })

			r = replyFrom(t, nil, "tools/call", frame)
			message := assertFails(t, r, func(r *Response) { r.AssertNoStructuredContent() })
			if !strings.Contains(message, "expected no structured content") {
				t.Fatalf("AssertNoStructuredContent failed with %q", message)
			}
		})
	}
}

// TestNoStructuredContentHoldsOnlyWhenAbsent asserts the negative assertion says
// what it means: it holds for a result that carries no such member and for an
// error reply, which carries no result at all.
func TestNoStructuredContentHoldsOnlyWhenAbsent(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"boom"}}`,
	} {
		r := replyFrom(t, nil, "tools/call", frame)
		assertHolds(t, r, func(r *Response) { r.AssertNoStructuredContent() })
	}
}

// TestStructuredContentRefusesAnInvalidExpectation asserts an expectation that
// is not one JSON value cannot be compared against anything. A caller may hand
// the assertions a json.RawMessage, and bytes that merely begin with a value
// must not be read as that value.
func TestStructuredContentRefusesAnInvalidExpectation(t *testing.T) {
	r := replyFrom(t, nil, "tools/call", `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"a":1}}}`)
	message := assertFails(t, r, func(r *Response) {
		r.AssertStructuredContent(json.RawMessage(`{"a":1}]`))
	})
	if !strings.Contains(message, "structured content") {
		t.Fatalf("failure message = %q", message)
	}
}

// TestJSONEqualityRequiresOneWholeValue asserts the comparison reads exactly one
// JSON value and nothing after it. The decoder's More reports whether another
// element of an array or object follows, which is not the same question: it
// answers no at a closing bracket, so trailing tokens would slip past.
func TestJSONEqualityRequiresOneWholeValue(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "a trailing closing bracket", raw: `{"a":1}]`},
		{name: "a trailing closing brace", raw: `[1,2]}`},
		{name: "a second value", raw: `{"a":1} {"a":2}`},
		{name: "a trailing comma", raw: `{"a":1},`},
		{name: "a trailing scalar", raw: `1 2`},
		{name: "a trailing bracket after a scalar", raw: `1]`},
		{name: "a stray token", raw: `{"a":1} nope`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			if _, ok := decodeJSONValue(raw); ok {
				t.Fatalf("decodeJSONValue accepted %q", tc.raw)
			}
			if _, ok := canonicalJSON(raw); ok {
				t.Fatalf("canonicalJSON accepted %q", tc.raw)
			}
			if _, ok := comparableJSON(raw); ok {
				t.Fatalf("comparableJSON accepted %q", tc.raw)
			}
			// Equality must never hold for bytes that are not one value, in
			// either position.
			if jsonEqual(raw, map[string]any{"a": 1}) || jsonEqual(map[string]any{"a": 1}, raw) {
				t.Fatalf("jsonEqual accepted %q", tc.raw)
			}
			// Nor against itself: two unreadable expectations are not equal.
			if jsonEqual(raw, raw) {
				t.Fatalf("jsonEqual held %q against itself", tc.raw)
			}
		})
	}
}

// TestJSONEqualityAcceptsOneValueWithWhitespace asserts the stricter reading
// still accepts what it should: one value surrounded by insignificant
// whitespace, in every JSON type.
func TestJSONEqualityAcceptsOneValueWithWhitespace(t *testing.T) {
	tests := []struct {
		raw  string
		want any
	}{
		{raw: " {\"a\":1}\n", want: map[string]any{"a": 1}},
		{raw: "\t[1,2] ", want: []int{1, 2}},
		{raw: " \"ok\" ", want: "ok"},
		{raw: " 42\n", want: 42},
		{raw: " true ", want: true},
		{raw: " null ", want: nil},
	}
	for _, tc := range tests {
		t.Run(strings.TrimSpace(tc.raw), func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			if _, ok := decodeJSONValue(raw); !ok {
				t.Fatalf("decodeJSONValue refused %q", tc.raw)
			}
			if !jsonEqual(raw, tc.want) {
				t.Fatalf("jsonEqual(%q, %#v) = false", tc.raw, tc.want)
			}
		})
	}
}
