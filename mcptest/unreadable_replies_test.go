package mcptest

import (
	"strings"
	"testing"
)

// This file drives the assertions against replies they cannot read: no reply at
// all, a reply with no result, a result that is not an object, an isError that
// is not a boolean. An assertion that holds against such a reply certifies
// nothing, and the most used one, AssertOk, is the one a test leans on to say
// the call worked.

// okFrame wraps a result in a success reply frame.
func okFrame(result string) string {
	return `{"jsonrpc":"2.0","id":1,"result":` + result + `}`
}

// TestSuccessAssertionsRefuseAReplyTheyCannotRead asserts AssertOk and
// AssertHasNoErrors fail, naming the reason, on every reply that cannot be read
// as a successful result, and that AssertError on the same replies does not
// mistake the unreadable reply for an error either.
func TestSuccessAssertionsRefuseAReplyTheyCannotRead(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReason string
	}{
		{"no reply was produced", ``, "no reply was produced"},
		{"neither result nor error", `{"jsonrpc":"2.0","id":1}`, "neither a result nor an error"},
		{"result is null", okFrame(`null`), "result is not an object: null"},
		{"result is an array", okFrame(`[1]`), "result is not an object: [1]"},
		{"result is a string", okFrame(`"x"`), `result is not an object: "x"`},
		{"isError is a string", okFrame(`{"content":[{"type":"text","text":"bad"}],"isError":"true"}`), `isError is not a boolean: "true"`},
		{"isError is a number", okFrame(`{"content":[{"type":"text","text":"bad"}],"isError":1}`), "isError is not a boolean: 1"},
		{"isError is null", okFrame(`{"content":[],"isError":null}`), "isError is not a boolean: null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, assertion := range []struct {
				name string
				fn   func(*Response)
			}{
				{"AssertOk", func(r *Response) { r.AssertOk() }},
				{"AssertHasNoErrors", func(r *Response) { r.AssertHasNoErrors() }},
				{"AssertError", func(r *Response) { r.AssertError() }},
			} {
				r := replyFrom(t, nil, "tools/call", tc.frame)
				message := assertFails(t, r, assertion.fn)
				if !strings.Contains(message, tc.wantReason) {
					t.Fatalf("%s failure message = %q, want it to say %q", assertion.name, message, tc.wantReason)
				}
			}
		})
	}

	// The replies a success assertion certifies: an object result, with or
	// without the flag, where the flag is a boolean.
	for _, result := range []string{`{}`, `{"content":[],"isError":false}`, `{"content":[{"type":"text","text":"ok"}]}`, `{"protocolVersion":"2025-06-18"}`} {
		r := replyFrom(t, nil, "tools/call", okFrame(result))
		assertHolds(t, r, func(r *Response) { r.AssertOk().AssertHasNoErrors() })
	}
}

// TestNegativeContentAssertionsRefuseAReplyWithoutAResult asserts the negative
// content assertions do not hold on a reply that has no content to inspect: a
// "did not see" or a "carries no structured content" said of no reply, of a
// protocol error, or of a result that is not an object states nothing about
// the tool, which may never have run.
func TestNegativeContentAssertionsRefuseAReplyWithoutAResult(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReason string
	}{
		{"no reply was produced", ``, "no reply was produced"},
		{"a protocol error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Tool [typo] not found."}}`, "protocol error -32602: Tool [typo] not found."},
		{"result is null", okFrame(`null`), "result is not an object: null"},
		{"result is an array", okFrame(`["secret"]`), `result is not an object: ["secret"]`},
		{"result is a string", okFrame(`"secret"`), `result is not an object: "secret"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, assertion := range []struct {
				name string
				fn   func(*Response)
			}{
				{"AssertDontSeeText", func(r *Response) { r.AssertDontSeeText("secret") }},
				{"AssertNoStructuredContent", func(r *Response) { r.AssertNoStructuredContent() }},
			} {
				r := replyFrom(t, nil, "tools/call", tc.frame)
				message := assertFails(t, r, assertion.fn)
				if !strings.Contains(message, tc.wantReason) {
					t.Fatalf("%s failure message = %q, want it to say %q", assertion.name, message, tc.wantReason)
				}
			}
		})
	}
}

// TestAssertDontSeeTextReadsTheWholeResult asserts the negative text assertion
// sees every value the result carries, wherever the specification lets a tool
// put it: a resource link's uri, name and description, an embedded resource,
// the structured content, the metadata, a content member that is not an array,
// and a text that is not a string. AssertText finds the same content items.
func TestAssertDontSeeTextReadsTheWholeResult(t *testing.T) {
	cases := []struct {
		name   string
		method string
		result string
		// text is what must be seen; it is the unwanted text for
		// AssertDontSeeText and, when seeable is true, the expected text for
		// AssertText.
		text    string
		seeable bool
	}{
		{"a resource link uri", "tools/call", `{"content":[{"type":"resource_link","uri":"file:///secret-token","name":"n"}]}`, "secret-token", true},
		{"a resource link name", "tools/call", `{"content":[{"type":"resource_link","uri":"file:///x","name":"secret-name"}]}`, "secret-name", true},
		{"a resource link description", "tools/call", `{"content":[{"type":"resource_link","uri":"file:///x","name":"n","description":"secret description"}]}`, "secret description", true},
		{"a resource link title", "tools/call", `{"content":[{"type":"resource_link","uri":"file:///x","name":"n","title":"secret title"}]}`, "secret title", true},
		{"an embedded resource uri", "tools/call", `{"content":[{"type":"resource","resource":{"uri":"file:///secret-path","text":"x"}}]}`, "secret-path", true},
		{"an embedded resource text", "tools/call", `{"content":[{"type":"resource","resource":{"uri":"file:///x","text":"secret body"}}]}`, "secret body", true},
		{"an image payload", "tools/call", `{"content":[{"type":"image","data":"c2VjcmV0","mimeType":"image/png"}]}`, "c2VjcmV0", true},
		{"a text that is not a string", "tools/call", `{"content":[{"type":"text","text":5}],"isError":false}`, "5", true},
		{"a content member that is not an array", "tools/call", `{"content":"secret","isError":false}`, "secret", false},
		{"a content item that is not an object", "tools/call", `{"content":["secret"],"isError":false}`, "secret", false},
		{"structured content", "tools/call", `{"content":[],"structuredContent":{"token":"secret-value"}}`, "secret-value", false},
		{"nested structured content", "tools/call", `{"content":[],"structuredContent":{"auth":{"tokens":["a","secret-value"]}}}`, "secret-value", false},
		{"a structured number", "tools/call", `{"content":[],"structuredContent":{"pin":4321}}`, "4321", false},
		{"result metadata", "tools/call", `{"content":[],"_meta":{"trace":"secret-trace"}}`, "secret-trace", false},
		{"content item metadata", "tools/call", `{"content":[{"type":"text","text":"ok","_meta":{"k":"secret-meta"}}]}`, "secret-meta", false},
		{"a prompt message", "prompts/get", `{"messages":[{"role":"user","content":{"type":"resource_link","uri":"file:///secret-link","name":"n"}}]}`, "secret-link", true},
		{"a prompt message that is not an object", "prompts/get", `{"messages":["secret"]}`, "secret", false},
		{"a resource blob", "resources/read", `{"contents":[{"uri":"file:///x","blob":"c2VjcmV0"}]}`, "c2VjcmV0", true},
		{"a resource uri", "resources/read", `{"contents":[{"uri":"file:///secret-path","text":"x"}]}`, "secret-path", true},
		{"a tool error message", "tools/call", `{"content":[{"type":"text","text":"secret failure"}],"isError":true}`, "secret failure", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := replyFrom(t, nil, tc.method, okFrame(tc.result))
			message := assertFails(t, r, func(r *Response) { r.AssertDontSeeText(tc.text) })
			if !strings.Contains(message, "did not expect to see "+jsonString(tc.text)) {
				t.Fatalf("AssertDontSeeText failure message = %q", message)
			}
			if !strings.Contains(message, tc.text) {
				t.Fatalf("AssertDontSeeText failure message = %q, want it to show where the text was seen", message)
			}
			if tc.seeable {
				assertHolds(t, r, func(r *Response) { r.AssertText(tc.text) })
			}
		})
	}

	// A key is structure, not content: a result does not "say" the names of
	// its members, and the vocabulary of the wire shape would otherwise make
	// a short unwanted text fail every reply.
	r := replyFrom(t, nil, "tools/call", okFrame(`{"content":[],"structuredContent":{"secret":"v"}}`))
	assertHolds(t, r, func(r *Response) { r.AssertDontSeeText("secret") })
	// Nor is a text seen that is not there.
	r = replyFrom(t, nil, "tools/call", okFrame(`{"content":[{"type":"text","text":"ok"}],"structuredContent":{"a":1}}`))
	assertHolds(t, r, func(r *Response) { r.AssertDontSeeText("secret", "2") })
}

// TestAssertServerNameDistinguishesAbsentFromEmpty asserts AssertServerName
// fails when the reply does not carry a string serverInfo.name, whatever name
// is expected: an empty expectation must not be met by a missing member.
func TestAssertServerNameDistinguishesAbsentFromEmpty(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReason string
	}{
		{"no reply was produced", ``, "no reply was produced"},
		{"a protocol error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"bad"}}`, "protocol error -32600: bad"},
		{"result is not an object", okFrame(`null`), "result is not an object"},
		{"no serverInfo", okFrame(`{"protocolVersion":"2025-06-18"}`), "carries no serverInfo.name"},
		{"serverInfo is not an object", okFrame(`{"serverInfo":"demo"}`), "carries no serverInfo.name"},
		{"serverInfo without a name", okFrame(`{"serverInfo":{"version":"1"}}`), "carries no serverInfo.name"},
		{"a null name", okFrame(`{"serverInfo":{"name":null}}`), "serverInfo.name is not a string: null"},
		{"a numeric name", okFrame(`{"serverInfo":{"name":5}}`), "serverInfo.name is not a string: 5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range []string{"", "demo"} {
				message := assertFails(t, replyFrom(t, nil, "initialize", tc.frame), func(r *Response) { r.AssertServerName(want) })
				if !strings.Contains(message, tc.wantReason) {
					t.Fatalf("AssertServerName(%q) failure = %q, want it to say %q", want, message, tc.wantReason)
				}
			}
		})
	}

	assertHolds(t, replyFrom(t, nil, "initialize", okFrame(`{"serverInfo":{"name":""}}`)), func(r *Response) { r.AssertServerName("") })
	assertHolds(t, replyFrom(t, nil, "initialize", okFrame(`{"serverInfo":{"name":"demo"}}`)), func(r *Response) { r.AssertServerName("demo") })
	message := assertFails(t, replyFrom(t, nil, "initialize", okFrame(`{"serverInfo":{"name":"demo2"}}`)), func(r *Response) { r.AssertServerName("demo") })
	if !strings.Contains(message, `serverInfo.name = "demo2", want "demo"`) {
		t.Fatalf("failure = %q", message)
	}
}
