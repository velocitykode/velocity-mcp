package transport

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// TestSubscriptionStreamsForEveryAcceptThatAllowsIt asserts a subscription is
// served as an event stream whenever the client can read one, including the
// "application/json, text/event-stream" every conformant client sends. The
// acknowledgement notification precedes the result, and only a stream can carry
// both, so a buffered reply would drop a frame the client is owed.
func TestSubscriptionStreamsForEveryAcceptThatAllowsIt(t *testing.T) {
	tests := []struct {
		name   string
		accept string
	}{
		{"event stream only", "text/event-stream"},
		{"json and event stream", "application/json, text/event-stream"},
		{"event stream with quality values", "application/json;q=0.9, text/event-stream;q=1.0"},
		{"wildcard", "*/*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := modernBody(7, "subscriptions/listen")
			w := serve(t, body, append(mcpHeaders(t, body), "Accept", tt.accept)...)

			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
				t.Fatalf("Content-Type = %q, want an event stream", ct)
			}
			payloads := sseFrames(w.Body.String())
			if len(payloads) != 2 {
				t.Fatalf("want an acknowledgement and a result, got %d frames: %s", len(payloads), w.Body.String())
			}
			var ack map[string]any
			if err := json.Unmarshal([]byte(payloads[0]), &ack); err != nil {
				t.Fatalf("decode acknowledgement %q: %v", payloads[0], err)
			}
			if ack["method"] != "notifications/subscriptions/acknowledged" {
				t.Fatalf("first frame = %v", ack)
			}
		})
	}
}

// TestNonSubscriptionKeepsTheBufferedReply asserts the wider streaming rule is
// confined to subscriptions: an ordinary request from a client that accepts
// both framings is still answered with JSON, which is the lighter
// representation and the one that can still carry the session header.
func TestNonSubscriptionKeepsTheBufferedReply(t *testing.T) {
	body := modernBody(1, "tools/list")
	w := serve(t, body, append(mcpHeaders(t, body), "Accept", "application/json, text/event-stream")...)

	if ct := w.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}
}

// TestEmptyBodyValueIsStillCompared asserts a member the body states as an
// empty string is a stated value: a header carrying anything else contradicts
// it and the request is refused. Treating an empty string as "not stated" would
// let a header claim a tool the body never named.
func TestEmptyBodyValueIsStillCompared(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers []string
		wantMsg string
	}{
		{
			name: "empty tool name",
			body: modernBody(1, "tools/call", `"name":"","arguments":{}`),
			headers: []string{
				HeaderProtocolVersion, "2026-07-28",
				HeaderMethod, "tools/call",
				HeaderName, "add",
			},
			wantMsg: "Header mismatch: The [Mcp-Name] header value [add] does not match the request body value [].",
		},
		{
			name: "empty protocol version",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			headers: []string{
				HeaderProtocolVersion, "2026-07-28",
				HeaderMethod, "tools/list",
			},
			wantMsg: "Header mismatch: The [MCP-Protocol-Version] header value [2026-07-28] does not match the request body value [].",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serve(t, tt.body, tt.headers...))
			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != tt.wantMsg {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.wantMsg)
			}
		})
	}
}

// TestIllTypedBodyValueIsNotCompared asserts the other side of the same rule: a
// member the body does not state as a string cannot be contradicted, so the
// header guard stands aside and the handler reports the malformed body in its
// own terms.
func TestIllTypedBodyValueIsNotCompared(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":123,"arguments":{}`)
	w := serve(t, body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, "add")

	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil {
		t.Fatalf("want the handler's own complaint about the body, got %s", w.Body.String())
	}
	if resp.Error.Code == jsonrpc.CodeHeaderMismatch {
		t.Fatalf("the header guard answered for a body it cannot judge: %s", w.Body.String())
	}
	if resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d (body %s)", resp.Error.Code, jsonrpc.CodeInvalidParams, w.Body.String())
	}
	if resp.Error.Message != "Missing [name] parameter." {
		t.Fatalf("message = %q, want the handler's own", resp.Error.Message)
	}
}

// TestUnpaddedBase64HeaderIsDecoded asserts an Mcp-Name whose base64 payload
// arrives without its padding is still read as the value it encodes. A peer
// that trims the padding is stating a name this server can recover, and
// comparing the payload as literal text instead would refuse a request that
// mirrors its body correctly.
//
// The names below are chosen so their byte lengths are not multiples of three:
// one pads to a single "=", the other to two, so the padded and unpadded
// spellings genuinely differ and the unpadded one only decodes if the server
// accepts it.
func TestUnpaddedBase64HeaderIsDecoded(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		wantPad int
	}{
		{"one padding character", "é ad", 1},
		{"two padding characters", "é a", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			padded := EncodeHeaderValue(tt.tool)
			if !strings.HasPrefix(padded, "=?base64?") || !strings.HasSuffix(padded, "?=") {
				t.Fatalf("expected a wrapped header value, got %q", padded)
			}
			payload := strings.TrimSuffix(strings.TrimPrefix(padded, "=?base64?"), "?=")
			if got := strings.Count(payload, "="); got != tt.wantPad {
				t.Fatalf("payload %q carries %d padding characters, want %d", payload, got, tt.wantPad)
			}

			raw := base64.RawStdEncoding.EncodeToString([]byte(tt.tool))
			unpadded := "=?base64?" + raw + "?="
			if strings.Contains(raw, "=") {
				t.Fatalf("raw encoding must not pad: %q", raw)
			}
			if unpadded == padded {
				t.Fatalf("the two spellings are identical (%q), so the test proves nothing", padded)
			}

			got, err := DecodeHeaderValue(unpadded)
			if err != nil {
				t.Fatalf("unpadded header %q was refused: %v", unpadded, err)
			}
			if got != tt.tool {
				t.Fatalf("decoded %q, want %q", got, tt.tool)
			}

			// The wire half: the guard must accept the unpadded header as
			// mirroring the body, and let the request through to the handler,
			// which reports the unknown tool by the exact name the body named.
			body := modernBody(1, "tools/call", `"name":`+quote(t, tt.tool)+`,"arguments":{}`)
			w := serve(t, body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, unpadded)

			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error == nil {
				t.Fatalf("want the handler's complaint about an unknown tool, got %s", w.Body.String())
			}
			if resp.Error.Code == jsonrpc.CodeHeaderMismatch {
				t.Fatalf("an unpadded but correct name was refused: %s", w.Body.String())
			}
			if want := "Tool [" + tt.tool + "] not found."; resp.Error.Message != want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, want)
			}
		})
	}
}

// quote renders a value as a JSON string literal for embedding in a test body.
func quote(t *testing.T, value string) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %q: %v", value, err)
	}
	return string(b)
}

// TestMalformedHeaderValuesAreRefused asserts a header value the encoding
// cannot express fails the request even when the body spells it the same way.
// Equality is not enough: a wrapper that decodes to nothing, or a raw value that
// should have been wrapped, names the primitive in a form no encoder produces,
// and an intermediary routing on the header would resolve it differently from
// the server reading the body.
func TestMalformedHeaderValuesAreRefused(t *testing.T) {
	// Every case states the same value in the header and in the body, so only a
	// rule beyond equality can refuse it. The headers are derived from the body,
	// which mirrors it verbatim.
	const junkVersion = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":` +
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28\u00df","io.modelcontextprotocol/clientCapabilities":{}}}}`

	tests := []struct {
		name   string
		body   string
		header string
		value  string
		want   string
	}{
		{
			name:   "undecodable wrapper on the name",
			body:   modernBody(1, "tools/call", `"name":"=?base64?not base64 at all?=","arguments":{}`),
			header: HeaderName,
			value:  "=?base64?not base64 at all?=",
			want:   "Header mismatch: The [Mcp-Name] header value declares a base64 encoding that does not decode.",
		},
		{
			name:   "sentinel-shaped junk on the name",
			body:   modernBody(1, "tools/call", `"name":"=?base64?%%%?=","arguments":{}`),
			header: HeaderName,
			value:  "=?base64?%%%?=",
			want:   "Header mismatch: The [Mcp-Name] header value declares a base64 encoding that does not decode.",
		},
		{
			name:   "raw non-ascii name",
			body:   modernBody(1, "tools/call", `"name":"wetter-f\u00fcr-k\u00f6ln","arguments":{}`),
			header: HeaderName,
			value:  "wetter-für-köln",
			want:   "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
		},
		{
			name:   "raw non-ascii method",
			body:   modernBody(1, "tools/cäll"),
			header: HeaderMethod,
			value:  "tools/cäll",
			want:   "Header mismatch: The [Mcp-Method] header value is not a valid header value.",
		},
		{
			name:   "raw non-ascii protocol version",
			body:   junkVersion,
			header: HeaderProtocolVersion,
			value:  "2026-07-28ß",
			want:   "Header mismatch: The [MCP-Protocol-Version] header value is not a valid header value.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := server.InspectMessage([]byte(tt.body))
			if !ok {
				t.Fatalf("body is not a request: %s", tt.body)
			}
			mirrored := map[string]string{
				HeaderProtocolVersion: info.ProtocolVersion,
				HeaderMethod:          info.Method,
				HeaderName:            info.Name,
			}
			if mirrored[tt.header] != tt.value {
				t.Fatalf("the body states %q for %s, not %q, so the test proves nothing", mirrored[tt.header], tt.header, tt.value)
			}
			w := serve(t, tt.body, replaceHeader(mcpHeaders(t, tt.body), tt.header, tt.value)...)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error == nil || resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("error = %+v, want code %d", resp.Error, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != tt.want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.want)
			}
			if strings.Contains(resp.Error.Message, tt.value) {
				t.Fatalf("the refusal echoes the offending value: %q", resp.Error.Message)
			}
		})
	}
}

// TestHeaderValuePaddedWithWiderWhitespaceIsRefused asserts a header padded with
// whitespace beyond the SP and HTAB that RFC 9110 section 5.6.3 defines around a
// field value is refused, not quietly trimmed back to the value the body states.
//
// Those bytes are part of the field value: an HTTP server removes SP and HTAB
// and hands everything else on untouched (TestNetHTTPKeepsWiderWhitespaceInAHeader
// measures that over a real connection). Trimming them here would accept a
// header no encoder could have written, and an intermediary routing on the
// header would read the padded spelling while the server read the body.
func TestHeaderValuePaddedWithWiderWhitespaceIsRefused(t *testing.T) {
	call := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)
	const version = "2026-07-28"

	tests := []struct {
		name   string
		body   string
		header string
		value  string
		want   string
	}{
		{
			name: "no-break space before the name", body: call,
			header: HeaderName, value: "\u00a0add",
			want: "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
		},
		{
			name: "ideographic space after the name", body: call,
			header: HeaderName, value: "add\u3000",
			want: "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
		},
		{
			name: "next line before the name", body: call,
			header: HeaderName, value: "\u0085add",
			want: "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
		},
		{
			name: "narrow no-break space around a wrapped name", body: call,
			header: HeaderName, value: "\u202f" + EncodeHeaderValue("add") + "\u202f",
			want: "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
		},
		{
			name: "line feed after the name", body: call,
			header: HeaderName, value: "add\n",
			want: "Header mismatch: The [Mcp-Name] header value is not a valid header value.",
		},
		{
			name: "line separator after the method", body: call,
			header: HeaderMethod, value: "tools/call\u2028",
			want: "Header mismatch: The [Mcp-Method] header value is not a valid header value.",
		},
		{
			name: "medium mathematical space before the method", body: call,
			header: HeaderMethod, value: "\u205ftools/call",
			want: "Header mismatch: The [Mcp-Method] header value is not a valid header value.",
		},
		{
			name: "ogham space mark before the protocol version", body: call,
			header: HeaderProtocolVersion, value: "\u1680" + version,
			want: "Header mismatch: The [MCP-Protocol-Version] header value is not a valid header value.",
		},
		{
			name: "en quad after the protocol version", body: call,
			header: HeaderProtocolVersion, value: version + "\u2000",
			want: "Header mismatch: The [MCP-Protocol-Version] header value is not a valid header value.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body, replaceHeader(mcpHeaders(t, tt.body), tt.header, tt.value)...)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error == nil || resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("error = %+v, want code %d", resp.Error, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != tt.want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.want)
			}
			if strings.Contains(resp.Error.Message, tt.value) {
				t.Fatalf("the refusal echoes the offending value: %q", resp.Error.Message)
			}
		})
	}
}

// TestLegacyRequestWithAMalformedVersionHeaderIsRefused asserts the same rule
// reaches the exemption that carries clients predating params._meta. A version
// header no peer could have written states nothing in any revision, so it is
// refused rather than read as a request declaring no version; a well-formed
// header naming an earlier revision is still served, which is what the exemption
// is for.
func TestLegacyRequestWithAMalformedVersionHeaderIsRefused(t *testing.T) {
	// No params._meta: the body is a legacy one whatever its header says.
	const legacy = `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name: "no-break space before a discovery revision", value: "\u00a02026-07-28",
			want: "Header mismatch: The [MCP-Protocol-Version] header value is not a valid header value.",
		},
		{
			name: "ideographic space after a discovery revision", value: "2026-07-28\u3000",
			want: "Header mismatch: The [MCP-Protocol-Version] header value is not a valid header value.",
		},
		{
			name: "raw non-ascii in an unknown revision", value: "2026-07-28ß",
			want: "Header mismatch: The [MCP-Protocol-Version] header value is not a valid header value.",
		},
		{
			// A well-formed header naming a discovery revision contradicts a body
			// that declares none, which is the complaint the guard already made.
			name: "a discovery revision the body does not declare", value: "2026-07-28",
			want: "Header mismatch: The [MCP-Protocol-Version] header declares protocol version [2026-07-28] but the request body declares no protocol version.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serve(t, legacy, HeaderProtocolVersion, tt.value))
			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			if resp.Error.Message != tt.want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.want)
			}
			if got := string(resp.ID.Raw()); got != "5" {
				t.Fatalf("id = %s, want 5", got)
			}
		})
	}
}

// TestLegacyRequestKeepsItsOwnVersionHeader asserts the guard still stands aside
// for the clients the exemption exists for: a well-formed header naming a
// revision the initialize handshake negotiates, or padded with the whitespace
// RFC 9110 permits, leaves the request served as it always was.
func TestLegacyRequestKeepsItsOwnVersionHeader(t *testing.T) {
	const legacy = `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`

	for _, value := range []string{"2025-06-18", " 2025-06-18 ", "2025-11-25"} {
		t.Run(value, func(t *testing.T) {
			w := serve(t, legacy, HeaderProtocolVersion, value)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if resp := decodeResponse(t, w.Body.Bytes()); resp.Error != nil {
				t.Fatalf("legacy request refused: %+v", resp.Error)
			}
		})
	}
}

// TestOptionalWhitespaceAroundAHeaderValueIsAccepted asserts the other half of
// the same rule: the SP and HTAB that RFC 9110 section 5.6.3 permits around a
// field value are not part of it, so a peer padding its headers that way still
// mirrors its body and is served.
func TestOptionalWhitespaceAroundAHeaderValueIsAccepted(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)

	tests := []struct {
		name    string
		headers []string
	}{
		{"space before the name", []string{HeaderName, " add"}},
		{"tab after the name", []string{HeaderName, "add\t"}},
		{"both around the method", []string{HeaderMethod, " \ttools/call \t"}},
		{"space around the protocol version", []string{HeaderProtocolVersion, " 2026-07-28 "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, body, replaceHeader(mcpHeaders(t, body), tt.headers[0], tt.headers[1])...)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error != nil {
				t.Fatalf("padded header refused: %+v", resp.Error)
			}
		})
	}
}

// TestNetHTTPKeepsWiderWhitespaceInAHeader drives the guard over a real
// connection and asserts the handler sees the padded value byte for byte. It is
// the measurement the refusal above rests on: an HTTP server strips SP and HTAB
// alone, so nothing but this guard stands between a padded header and a request
// served as though its headers mirrored its body.
func TestNetHTTPKeepsWiderWhitespaceInAHeader(t *testing.T) {
	const padded = "\u00a0add\u3000"
	srv := newTestServer(t)

	var seen string
	r := router.New()
	r.Post("/mcp", func(c *router.Context) error {
		seen = c.Request.Header.Get(HeaderName)
		return Handler(srv)(c)
	})
	ts := httptest.NewServer(r)
	defer ts.Close()

	body := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", contentTypeJSON)
	req.Header.Set(HeaderProtocolVersion, "2026-07-28")
	req.Header.Set(HeaderMethod, "tools/call")
	req.Header.Set(HeaderName, padded)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if seen != padded {
		t.Fatalf("the handler saw %q, want the padded value %q verbatim", seen, padded)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	answer := decodeResponse(t, raw)
	if answer.Error == nil || answer.Error.Code != jsonrpc.CodeHeaderMismatch {
		t.Fatalf("error = %+v, want code %d", answer.Error, jsonrpc.CodeHeaderMismatch)
	}
	const want = "Header mismatch: The [Mcp-Name] header value is not a valid header value."
	if answer.Error.Message != want {
		t.Fatalf("message = %q, want %q", answer.Error.Message, want)
	}
}

// TestHeaderMismatchResponsePinsItsContentType asserts the refusal, whose body
// echoes request-controlled text back to the client, is served with the same
// content-type pinning as every other reply this transport writes, so no
// browser can be talked into sniffing it as something executable.
func TestHeaderMismatchResponsePinsItsContentType(t *testing.T) {
	body := modernBody(1, "tools/list")
	w := serve(t, body, replaceHeader(mcpHeaders(t, body), HeaderMethod, "<script>")...)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, contentTypeJSON) {
		t.Fatalf("Content-Type = %q, want JSON", ct)
	}
}

// TestMalformedParamsPassesTheHeaderGuard asserts a body whose params member is
// not an object states no protocol metadata, so the guard treats it as legacy
// and lets the server answer with the JSON-RPC error the client can act on
// instead of a header complaint about a body that was never well formed.
func TestMalformedParamsPassesTheHeaderGuard(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":"nope"}`
	w := serve(t, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("want invalid params, got %s", w.Body.String())
	}
	if got := string(resp.ID.Raw()); got != "9" {
		t.Fatalf("id = %s, want 9", got)
	}
}

// serveWithExtraLine runs the handler over a body whose headers mirror it,
// plus one further field line for the named header.
func serveWithExtraLine(t *testing.T, body, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	c, w := postContext(t, body, mcpHeaders(t, body)...)
	c.Request.Header.Add(name, value)
	if err := Handler(newTestServer(t))(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return w
}

// TestAHeaderStatedTwiceIsRefused asserts a request carrying two field lines for
// the same MCP header is refused rather than read off the first one. RFC 9110
// section 5.3 lets a recipient join repeated field lines into one
// comma-separated value, so an intermediary routing on the header and the server
// reading the body would be looking at different values, which is the
// disagreement these headers exist to rule out. Two lines stating the same value
// are refused with the rest: what an intermediary joins is "v, v", which no
// encoder writes and no body spells.
func TestAHeaderStatedTwiceIsRefused(t *testing.T) {
	call := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)

	tests := []struct {
		name   string
		header string
		second string
	}{
		{"the protocol version repeated verbatim", HeaderProtocolVersion, "2026-07-28"},
		{"the protocol version contradicted", HeaderProtocolVersion, "2025-06-18"},
		{"the method repeated verbatim", HeaderMethod, "tools/call"},
		{"the method contradicted", HeaderMethod, "tools/list"},
		{"the name repeated verbatim", HeaderName, "add"},
		{"the name contradicted", HeaderName, "subtract"},
		{"the name contradicted in its wrapped form", HeaderName, EncodeHeaderValue("subtract")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := headerError(t, serveWithExtraLine(t, call, tt.header, tt.second))

			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			want := "Header mismatch: The [" + tt.header + "] header is stated more than once."
			if resp.Error.Message != want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, want)
			}
			if got := string(resp.ID.Raw()); got != "1" {
				t.Fatalf("id = %s, want 1", got)
			}
		})
	}
}

// TestALegacyRequestStatingItsVersionTwiceIsRefused asserts the same rule reaches
// the exemption: the version header is what settles which era a legacy body is
// read under, so a request stating it twice settles nothing and is refused
// rather than served as one declaring no version.
func TestALegacyRequestStatingItsVersionTwiceIsRefused(t *testing.T) {
	const legacy = `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{}}`

	for _, second := range []string{"2025-06-18", "2026-07-28"} {
		t.Run(second, func(t *testing.T) {
			c, w := postContext(t, legacy, HeaderProtocolVersion, "2025-06-18")
			c.Request.Header.Add(HeaderProtocolVersion, second)
			if err := Handler(newTestServer(t))(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			resp := headerError(t, w)
			if resp.Error.Code != jsonrpc.CodeHeaderMismatch {
				t.Fatalf("code = %d, want %d", resp.Error.Code, jsonrpc.CodeHeaderMismatch)
			}
			const want = "Header mismatch: The [" + HeaderProtocolVersion + "] header is stated more than once."
			if resp.Error.Message != want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, want)
			}
			if got := string(resp.ID.Raw()); got != "5" {
				t.Fatalf("id = %s, want 5", got)
			}
		})
	}
}
