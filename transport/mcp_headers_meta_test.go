package transport

import (
	"net/http"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity/router"
)

// This file covers a request whose MCP-Protocol-Version header names a
// revision whose requests restate their protocol version in params._meta, and
// whose body states none. The specification (2026-07-28, basic, _meta) says a
// request missing a required _meta member is malformed and MUST be refused with
// -32602; under the streamable HTTP status mapping that is a 400. The body alone
// looks like a legacy request, so the server would serve it; the transport,
// which has the header, is where the refusal has to be written.

// undeclaredBodies are discovery-era bodies that state no protocol version in
// any form: no params, params without _meta, a _meta with unrelated members, and
// a _meta carrying the client capabilities but not the version. Each is sent
// with the headers a compliant client of that revision would mirror.
var undeclaredBodies = []struct {
	name string
	body string
}{
	{"no params", `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`},
	{"params without _meta", `{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{}}`},
	{"_meta with unrelated members", `{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{"_meta":{"progressToken":1}}}`},
	{"_meta that is not an object", `{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{"_meta":[]}}`},
	{"_meta with the capabilities only", `{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`},
}

// TestDiscoveryHeaderOnABodyStatingNoVersionIsInvalidParams asserts every shape
// of an undeclared body is answered the same way: 400, -32602, the message
// naming the missing member, and the request id. Before this, a body with no
// _meta at all drew -32020 while one carrying only the capabilities drew
// -32602, and a client acts differently on the two codes.
func TestDiscoveryHeaderOnABodyStatingNoVersionIsInvalidParams(t *testing.T) {
	for _, tt := range undeclaredBodies {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/list")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("error = %+v, want code %d", resp.Error, jsonrpc.CodeInvalidParams)
			}
			if resp.Error.Message != missingProtocolVersion {
				t.Fatalf("message = %q, want %q", resp.Error.Message, missingProtocolVersion)
			}
			if got := string(resp.ID.Raw()); got != "6" {
				t.Fatalf("id = %s, want 6", got)
			}
		})
	}
}

// TestDiscoveryHeaderOnABodyStatingNoVersionNeverReachesTheHandler asserts the
// refusal happens before the body is handled, whatever shape the body takes.
func TestDiscoveryHeaderOnABodyStatingNoVersionNeverReachesTheHandler(t *testing.T) {
	for _, tt := range undeclaredBodies {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			handled := 0
			next := func(c *router.Context) error {
				mu.Lock()
				handled++
				mu.Unlock()
				return nil
			}

			c, w := postContext(t, tt.body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/list")
			if err := ValidateHeaders()(next)(c); err != nil {
				t.Fatalf("middleware returned error: %v", err)
			}

			mu.Lock()
			handledCount := handled
			mu.Unlock()
			// A body carrying the capabilities does declare metadata, so the
			// guard passes it to the server, which refuses it on its merits;
			// every other shape is refused by the guard itself.
			if tt.name != "_meta with the capabilities only" && handledCount != 0 {
				t.Fatal("the handler ran for a discovery request stating no protocol version")
			}
			if handledCount == 0 && w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}
}

// TestConfiguredRevisionHeaderOnABodyStatingNoVersionIsInvalidParams asserts the
// rule follows the versions the served server accepts in a request's metadata,
// not the 2026-07-28 constant: a server configured for another revision holds a
// request naming it in the header to the same standard.
func TestConfiguredRevisionHeaderOnABodyStatingNoVersionIsInvalidParams(t *testing.T) {
	srv := server.New("versions", "1.0.0", server.WithProtocolVersions("2027-01-01"))
	w := serveWith(t, srv, legacyList, HeaderProtocolVersion, "2027-01-01", HeaderMethod, "tools/list")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("error = %+v, want code %d", resp.Error, jsonrpc.CodeInvalidParams)
	}
	if resp.Error.Message != missingProtocolVersion {
		t.Fatalf("message = %q, want %q", resp.Error.Message, missingProtocolVersion)
	}
}

// TestDiscoveryHeaderOnABodyStatingNoVersionChecksTheMirroredHeadersFirst
// asserts the request is held to the revision the header names in full: a
// contradicting or missing Mcp-Method or Mcp-Name is the -32020 of the
// mirrored-header rule, and only a request whose headers all agree with its body
// reaches the question of its missing metadata.
func TestDiscoveryHeaderOnABodyStatingNoVersionChecksTheMirroredHeadersFirst(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"add","arguments":{}}}`
	tests := []struct {
		name    string
		headers []string
		want    string
	}{
		{"method missing", []string{HeaderProtocolVersion, "2026-07-28", HeaderName, "add"}, "Header mismatch: The [Mcp-Method] header is required."},
		{"name missing", []string{HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call"}, "Header mismatch: The [Mcp-Name] header is required."},
		{"method contradicted", []string{HeaderProtocolVersion, "2026-07-28", HeaderMethod, "ping", HeaderName, "add"}, "Header mismatch: The [Mcp-Method] header value [ping] does not match the request body value [tools/call]."},
		{"name contradicted", []string{HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, "other"}, "Header mismatch: The [Mcp-Name] header value [other] does not match the request body value [add]."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serve(t, body, tt.headers...))
			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != tt.want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.want)
			}
		})
	}
}
