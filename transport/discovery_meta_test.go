package transport

import (
	"net/http"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// missingProtocolVersion is the refusal a request that states no protocol
// version is answered with. It is the message the specification's _meta rules
// call for, spelled out here as the client reads it off the wire.
const missingProtocolVersion = "Invalid params: The request [_meta] is missing the required [io.modelcontextprotocol/protocolVersion] member."

// discoveryOnlyBodies are requests for the two methods the discovery handshake
// introduced, each stating no protocol metadata: an absent params member, an
// empty one, a metadata bag that is not an object, and one holding only
// unrelated keys. No client predating that metadata can be calling either
// method, so every one of these is a malformed request rather than an older
// client's.
var discoveryOnlyBodies = []struct {
	name string
	body string
}{
	{"discover with no params", `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`},
	{"discover with empty params", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`},
	{"discover with a list for metadata", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":[]}}`},
	{"discover with unrelated metadata", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"progressToken":7}}}`},
	{"listen with no params", `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`},
	{"listen with empty params", `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{}}`},
	{"listen with a list for metadata", `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"_meta":[]}}`},
}

// TestDiscoveryOnlyMethodMissingItsMetadataIsRefusedWithBadRequest asserts the
// refusal reaches an HTTP client as a 400, not as a 200 carrying an error
// object. The status mapping applies to a discovery-handshake request, and a
// method that exists only in that handshake is one whatever its _meta holds: a
// request the server answers with -32602 must not be dressed as a transport
// success because its metadata was too malformed to recognise the handshake by.
//
// The headers here mirror the body a compliant client would have sent, so the
// request reaches the method and is refused on its merits.
func TestDiscoveryOnlyMethodMissingItsMetadataIsRefusedWithBadRequest(t *testing.T) {
	for _, tt := range discoveryOnlyBodies {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body,
				HeaderProtocolVersion, "2026-07-28",
				HeaderMethod, methodOf(t, tt.body),
			)

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
			if got := string(resp.ID.Raw()); got != "1" {
				t.Fatalf("id = %s, want 1", got)
			}
		})
	}
}

// TestDiscoveryOnlyMethodWithoutMirroredHeadersIsRefused asserts the same
// request is held to the mirrored-header rules as well. A request for a method
// only the discovery handshake defines carries those headers by definition, so
// one arriving without them is refused by the guard rather than waved through
// on the exemption that carries clients predating both.
func TestDiscoveryOnlyMethodWithoutMirroredHeadersIsRefused(t *testing.T) {
	const want = "Header mismatch: The [" + HeaderProtocolVersion + "] header is required."

	for _, tt := range discoveryOnlyBodies {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serve(t, tt.body))

			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, want)
			}
			if got := string(resp.ID.Raw()); got != "1" {
				t.Fatalf("id = %s, want 1", got)
			}
		})
	}
}

// TestUnmetadatedSubscriptionIsRefusedOnTheStreamingPath asserts the refusal
// keeps its status when the client accepts an event stream, which is the
// framing every conformant client offers for a subscription. The status travels
// with the reply whichever framing carries it.
func TestUnmetadatedSubscriptionIsRefusedOnTheStreamingPath(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":9,"method":"subscriptions/listen","params":{}}`
	w := serve(t, body,
		HeaderProtocolVersion, "2026-07-28",
		HeaderMethod, "subscriptions/listen",
		"Accept", "application/json, text/event-stream",
	)

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
	if got := string(resp.ID.Raw()); got != "9" {
		t.Fatalf("id = %s, want 9", got)
	}
}

// TestMethodWithALegacyFormKeepsTheExemption asserts the rule above is confined
// to the two methods that have no earlier form. A method that predates the
// discovery handshake is still served to a client that states no protocol
// metadata and sends no mirrored headers, under the 200 those clients read.
func TestMethodWithALegacyFormKeepsTheExemption(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":[]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := serve(t, body)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if resp := decodeResponse(t, w.Body.Bytes()); resp.Error != nil {
				t.Fatalf("legacy request refused: %+v", resp.Error)
			}
		})
	}
}
