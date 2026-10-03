package mcptest

import (
	"strings"
	"testing"
)

// TestAssertNotRegisteredMatchesTheMessageExactly asserts the three
// not-registered assertions compare the error message whole: the message the
// server writes is fully known, so a reply that prepends, appends or respells
// it is not the reply the assertion names, and detail leaked after it is a
// defect the assertion must report rather than pass over.
func TestAssertNotRegisteredMatchesTheMessageExactly(t *testing.T) {
	helpers := []struct {
		name   string
		exact  string
		assert func(*Response)
	}{
		{"tool", "Tool [x] not found.", func(r *Response) { r.AssertToolNotRegistered("x") }},
		{"prompt", "Prompt [x] not found.", func(r *Response) { r.AssertPromptNotRegistered("x") }},
		{"resource", "Resource [x] not found.", func(r *Response) { r.AssertResourceNotRegistered("x") }},
	}
	for _, h := range helpers {
		t.Run(h.name, func(t *testing.T) {
			frame := func(message string) string {
				return `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":` + jsonString(message) + `}}`
			}
			assertHolds(t, replyFrom(t, nil, "m", frame(h.exact)), h.assert)
			for _, wrong := range []string{
				"X" + h.exact,
				h.exact + " extra detail leaked: /etc/passwd",
				strings.ToLower(h.exact),
				strings.TrimSuffix(h.exact, "."),
				"",
			} {
				message := assertFails(t, replyFrom(t, nil, "m", frame(wrong)), h.assert)
				if !strings.Contains(message, `(error "`+h.exact+`")`) || !strings.Contains(message, "got: "+wrong) {
					t.Fatalf("message %q: failure = %q, want it to show the exact message wanted and the one got", wrong, message)
				}
			}
		})
	}
}
