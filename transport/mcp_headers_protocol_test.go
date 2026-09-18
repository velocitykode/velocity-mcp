package transport

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity/router"
)

// This file covers the one question the mirrored-header guard has to settle
// before it can check anything: which protocol the request is made under. The
// answer comes from the header and the body together, because a request that
// declares a discovery-era revision in its header and nothing in its body is
// what an intermediary would authorize from the header while the server reads
// the body.

// legacyToolCall is a tools/call body of the initialize era: it declares no
// protocol metadata of its own.
const legacyToolCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{"a":1,"b":2}}}`

// TestDiscoveryHeaderWithNoBodyDeclarationIsRejected asserts a body that strips
// its protocol metadata cannot buy its way out of header validation. The headers
// say 2026-07-28 and name one tool; the body names another and declares nothing.
// Accepting it would let a gateway that authorizes calls from these headers
// approve a call the server then runs against a different target.
func TestDiscoveryHeaderWithNoBodyDeclarationIsRejected(t *testing.T) {
	const wantMessage = "Header mismatch: The [MCP-Protocol-Version] header declares protocol version " +
		"[2026-07-28] but the request body declares no protocol version."

	tests := []struct {
		name    string
		headers []string
	}{
		{
			name:    "headers name another tool than the body",
			headers: []string{HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, "harmless"},
		},
		{
			name:    "headers mirror the body faithfully",
			headers: []string{HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, "add"},
		},
		{
			name:    "protocol header on its own",
			headers: []string{HeaderProtocolVersion, "2026-07-28"},
		},
		{
			name:    "protocol header spelled in another case",
			headers: []string{"mcp-protocol-version", "2026-07-28", HeaderMethod, "tools/call", HeaderName, "add"},
		},
		{
			name:    "protocol header padded with whitespace",
			headers: []string{HeaderProtocolVersion, "  2026-07-28  ", HeaderMethod, "tools/call", HeaderName, "add"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serve(t, legacyToolCall, tt.headers...))
			if resp.Error.Code != -32020 {
				t.Fatalf("code = %d, want -32020", resp.Error.Code)
			}
			if resp.Error.Message != wantMessage {
				t.Fatalf("message = %q, want %q", resp.Error.Message, wantMessage)
			}
			if got := string(resp.ID.Raw()); got != "1" {
				t.Fatalf("id = %s, want 1", got)
			}
		})
	}
}

// TestDiscoveryHeaderWithNoBodyDeclarationNeverReachesTheHandler asserts the
// refusal happens before the body is handled: the mismatched call must have no
// effect at all, not merely a reported one.
func TestDiscoveryHeaderWithNoBodyDeclarationNeverReachesTheHandler(t *testing.T) {
	var mu sync.Mutex
	handled := 0
	next := func(c *router.Context) error {
		mu.Lock()
		handled++
		mu.Unlock()
		return nil
	}

	c, w := postContext(t, legacyToolCall,
		HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, "harmless")
	if err := ValidateHeaders()(next)(c); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if handled != 0 {
		t.Fatal("the handler ran for a request whose header and body declare different protocols")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestInitializeEraProtocolHeaderLeavesLegacyRequestsExempt asserts the
// consistency rule is scoped to the revisions that mirror their metadata into
// these headers. A client of an earlier revision sends MCP-Protocol-Version on
// every request after it initializes, and its bodies carry no metadata by
// design; refusing those would break every such client.
func TestInitializeEraProtocolHeaderLeavesLegacyRequestsExempt(t *testing.T) {
	versions := []string{"2025-11-25", "2025-06-18", ""}
	for _, version := range versions {
		name := version
		if name == "" {
			name = "no header"
		}
		t.Run(name, func(t *testing.T) {
			headers := []string{}
			if version != "" {
				headers = append(headers, HeaderProtocolVersion, version)
			}
			w := serve(t, legacyToolCall, headers...)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error != nil {
				t.Fatalf("legacy request refused: %+v", resp.Error)
			}
			if !strings.Contains(w.Body.String(), `"3"`) {
				t.Fatalf("the tool did not run: %s", w.Body.String())
			}
		})
	}
}

// TestUnsupportedProtocolHeaderIsRejected asserts a header naming a revision
// this server does not speak is refused with the status and the code the
// streamable HTTP transport prescribes, rather than admitted through the legacy
// exemption. Reading the exemption as "every version the server does not
// recognize" is what would let a request state any protocol at all and reach
// the handler with no header checked.
func TestUnsupportedProtocolHeaderIsRejected(t *testing.T) {
	versions := []string{"2099-01-01", "2027-01-01", "2025-03-26", "2024-11-05", "not-a-version"}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			resp := headerError(t, serve(t, legacyToolCall, HeaderProtocolVersion, version))
			if resp.Error.Code != -32022 {
				t.Fatalf("code = %d, want -32022", resp.Error.Code)
			}
			if resp.Error.Message != "Unsupported protocol version" {
				t.Fatalf("message = %q, want %q", resp.Error.Message, "Unsupported protocol version")
			}
			if got := string(resp.ID.Raw()); got != "1" {
				t.Fatalf("id = %s, want 1", got)
			}
			data, ok := resp.Error.Data.(map[string]any)
			if !ok {
				t.Fatalf("data = %#v, want an object", resp.Error.Data)
			}
			if data["requested"] != version {
				t.Fatalf("data.requested = %#v, want %q", data["requested"], version)
			}
			want := []any{"2026-07-28", "2025-11-25", "2025-06-18"}
			if !reflect.DeepEqual(data["supported"], want) {
				t.Fatalf("data.supported = %#v, want %#v", data["supported"], want)
			}
		})
	}
}

// TestUnsupportedProtocolHeaderNeverReachesTheHandler asserts the refusal stops
// the call: a request naming a protocol the server does not speak must have no
// effect, not merely a reported one.
func TestUnsupportedProtocolHeaderNeverReachesTheHandler(t *testing.T) {
	var mu sync.Mutex
	handled := 0
	next := func(c *router.Context) error {
		mu.Lock()
		handled++
		mu.Unlock()
		return nil
	}

	c, w := postContext(t, legacyToolCall, HeaderProtocolVersion, "2099-01-01")
	if err := ValidateHeaders()(next)(c); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if handled != 0 {
		t.Fatal("the handler ran for a request naming a protocol version the server does not speak")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestDiscoveryHeaderWithAMatchingBodyStillPasses asserts the rule only ever
// refuses a disagreement: a request that declares the revision in both places
// and mirrors the rest of the body is served as before.
func TestDiscoveryHeaderWithAMatchingBodyStillPasses(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)
	w := serve(t, body, mcpHeaders(t, body)...)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"3"`) {
		t.Fatalf("the tool did not run: %s", w.Body.String())
	}
}

// TestDiscoveryHeaderOnANonRequestIsIgnored asserts the rule does not turn a
// message the guard cannot correlate into a header complaint. A notification
// carries no id to answer, so it stays the server's to handle.
func TestDiscoveryHeaderOnANonRequestIsIgnored(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, http.StatusAccepted},
		{"malformed json", `{"jsonrpc":"2.0",`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/list")
			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantCode, w.Body.String())
			}
		})
	}
}
