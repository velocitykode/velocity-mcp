package transport

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity/router"
)

// This file covers the half of the legacy exemption that is not an exemption:
// a client of an initialize-era revision need not send Mcp-Method or Mcp-Name,
// but one it does send is compared with the body like any other. The
// specification requires a server that processes the body to refuse a request
// whose header values do not match it, with no carve-out for the revision the
// request is made under (2026-07-28, streamable HTTP, server validation).
//
// Every refusal below is the 400 and -32020 that rule prescribes, with the
// request id echoed so the client can match it to the call it made.

// legacyDanger is an initialize-era tools/call whose target a hostile client
// would want an intermediary to misread: the body names "add" while the
// headers, where they are sent, name something else.
const legacyDanger = `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"add","arguments":{"a":1,"b":2}}}`

// legacyHeaderCases are the hostile header sets a legacy body can be sent with,
// one per revision the exemption speaks for (no version header, 2025-06-18 and
// 2025-11-25), and the complaint the specification's rule requires for each.
var legacyHeaderCases = []struct {
	name    string
	headers []string
	want    string
}{
	{
		name:    "no version header, method contradicts the body",
		headers: []string{HeaderMethod, "tools/list", HeaderName, "add"},
		want:    "Header mismatch: The [Mcp-Method] header value [tools/list] does not match the request body value [tools/call].",
	},
	{
		name:    "no version header, name contradicts the body",
		headers: []string{HeaderMethod, "tools/call", HeaderName, "harmless_read"},
		want:    "Header mismatch: The [Mcp-Name] header value [harmless_read] does not match the request body value [add].",
	},
	{
		name:    "no version header, name alone contradicts the body",
		headers: []string{HeaderName, "harmless_read"},
		want:    "Header mismatch: The [Mcp-Name] header value [harmless_read] does not match the request body value [add].",
	},
	{
		name:    "2025-06-18, method contradicts the body",
		headers: []string{HeaderProtocolVersion, "2025-06-18", HeaderMethod, "ping", HeaderName, "add"},
		want:    "Header mismatch: The [Mcp-Method] header value [ping] does not match the request body value [tools/call].",
	},
	{
		name:    "2025-06-18, name contradicts the body",
		headers: []string{HeaderProtocolVersion, "2025-06-18", HeaderMethod, "tools/call", HeaderName, "harmless_read"},
		want:    "Header mismatch: The [Mcp-Name] header value [harmless_read] does not match the request body value [add].",
	},
	{
		name:    "2025-11-25, method contradicts the body",
		headers: []string{HeaderProtocolVersion, "2025-11-25", HeaderMethod, "tools/list", HeaderName, "harmless_read"},
		want:    "Header mismatch: The [Mcp-Method] header value [tools/list] does not match the request body value [tools/call].",
	},
	{
		name:    "2025-11-25, name contradicts the body in its wrapped form",
		headers: []string{HeaderProtocolVersion, "2025-11-25", HeaderMethod, "tools/call", HeaderName, "=?base64?aGFybWxlc3NfcmVhZA==?="},
		want:    "Header mismatch: The [Mcp-Name] header value [harmless_read] does not match the request body value [add].",
	},
	{
		name:    "name wrapped in a payload that does not decode",
		headers: []string{HeaderName, "=?base64?!!!?="},
		want:    "Header mismatch: The [Mcp-Name] header value declares a base64 encoding that does not decode.",
	},
	{
		name:    "name carrying bytes a field value may not",
		headers: []string{HeaderName, "héllo"},
		want:    "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
	},
	{
		name:    "method carrying a control character",
		headers: []string{HeaderMethod, "tools/call\x7f"},
		want:    "Header mismatch: The [Mcp-Method] header value is not a valid header value.",
	},
	{
		name:    "method differing in case only",
		headers: []string{HeaderProtocolVersion, "2025-11-25", HeaderMethod, "Tools/Call"},
		want:    "Header mismatch: The [Mcp-Method] header value [Tools/Call] does not match the request body value [tools/call].",
	},
}

// TestLegacyRequestHeadersThatContradictTheBodyAreRefused asserts a legacy body
// cannot be run under headers that describe a different call. Every case is
// answered 400 and -32020 naming the header at fault, with the request id.
func TestLegacyRequestHeadersThatContradictTheBodyAreRefused(t *testing.T) {
	for _, tt := range legacyHeaderCases {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serve(t, legacyDanger, tt.headers...))
			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != tt.want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.want)
			}
			if got := string(resp.ID.Raw()); got != "9" {
				t.Fatalf("id = %s, want 9", got)
			}
		})
	}
}

// TestLegacyRequestHeadersThatContradictTheBodyNeverReachTheHandler asserts the
// refusal is a refusal: the mismatched call has no effect at all. A header check
// that reported the disagreement after running the body would protect nothing.
func TestLegacyRequestHeadersThatContradictTheBodyNeverReachTheHandler(t *testing.T) {
	for _, tt := range legacyHeaderCases {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			handled := 0
			next := func(c *router.Context) error {
				mu.Lock()
				handled++
				mu.Unlock()
				return nil
			}

			c, w := postContext(t, legacyDanger, tt.headers...)
			if err := ValidateHeaders()(next)(c); err != nil {
				t.Fatalf("middleware returned error: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if handled != 0 {
				t.Fatal("the handler ran for a legacy request whose headers contradict its body")
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}
}

// TestLegacyRequestHeaderStatedTwiceIsRefused asserts a legacy body is refused
// when it states Mcp-Method or Mcp-Name more than once, exactly as a discovery
// request is: RFC 9110 section 5.3 lets a recipient join the lines into one
// value, so reading the first line would leave an intermediary and this server
// looking at different things.
func TestLegacyRequestHeaderStatedTwiceIsRefused(t *testing.T) {
	tests := []struct {
		name   string
		header string
		first  string
		second string
	}{
		{"the method, repeated verbatim", HeaderMethod, "tools/call", "tools/call"},
		{"the method, contradicted", HeaderMethod, "tools/call", "tools/list"},
		{"the name, repeated verbatim", HeaderName, "add", "add"},
		{"the name, contradicted", HeaderName, "add", "harmless_read"},
		{"the name, one line malformed", HeaderName, "=?base64?!!!?=", "harmless_read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := postContext(t, legacyDanger, tt.header, tt.first)
			c.Request.Header.Add(tt.header, tt.second)
			if err := Handler(newTestServer(t))(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			resp := headerError(t, w)
			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			want := "Header mismatch: The [" + tt.header + "] header is stated more than once."
			if resp.Error.Message != want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, want)
			}
			if got := string(resp.ID.Raw()); got != "9" {
				t.Fatalf("id = %s, want 9", got)
			}
		})
	}
}

// TestLegacyRequestHeadersThatMirrorTheBodyAreServed asserts the other half of
// the rule: a legacy client that does send the headers, correctly, is served, as
// is one that sends an Mcp-Name a method addressing no target has no use for.
// The exemption forgives absence; agreement was never in question.
func TestLegacyRequestHeadersThatMirrorTheBodyAreServed(t *testing.T) {
	const list = `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`

	tests := []struct {
		name    string
		body    string
		headers []string
		want    string
	}{
		{"method and name, no version", legacyDanger, []string{HeaderMethod, "tools/call", HeaderName, "add"}, `"3"`},
		{"method and wrapped name, 2025-06-18", legacyDanger, []string{HeaderProtocolVersion, "2025-06-18", HeaderMethod, "tools/call", HeaderName, "=?base64?YWRk?="}, `"3"`},
		{"method alone, 2025-11-25", legacyDanger, []string{HeaderProtocolVersion, "2025-11-25", HeaderMethod, "tools/call"}, `"3"`},
		{"name alone", legacyDanger, []string{HeaderName, "add"}, `"3"`},
		{"headers padded with optional whitespace", legacyDanger, []string{HeaderMethod, " tools/call ", HeaderName, "\tadd\t"}, `"3"`},
		{"a name header on a method with no target", list, []string{HeaderMethod, "tools/list", HeaderName, "anything"}, `"tools"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body, tt.headers...)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error != nil {
				t.Fatalf("legacy request refused: %+v", resp.Error)
			}
			if !strings.Contains(string(resp.Result), tt.want) {
				t.Fatalf("result %s does not carry %s", resp.Result, tt.want)
			}
		})
	}
}
