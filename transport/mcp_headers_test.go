package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity/router"
)

// modernBody builds a discovery-handshake request body for the given method and
// extra params.
func modernBody(id int, method string, extra ...string) string {
	parts := append([]string{
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`,
	}, extra...)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s}}`, id, method, strings.Join(parts, ","))
}

// mcpHeaders returns the headers a compliant client sends for a body, as
// alternating key/value pairs for postContext.
func mcpHeaders(t *testing.T, body string) []string {
	t.Helper()
	info, ok := server.InspectMessage([]byte(body))
	if !ok {
		t.Fatalf("body is not a request: %s", body)
	}
	headers := []string{HeaderProtocolVersion, info.ProtocolVersion, HeaderMethod, info.Method}
	if info.RequiresName && info.Name != "" {
		headers = append(headers, HeaderName, EncodeHeaderValue(info.Name))
	}
	return headers
}

// withoutHeader drops one key/value pair from an alternating header list.
func withoutHeader(headers []string, name string) []string {
	out := make([]string, 0, len(headers))
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == name {
			continue
		}
		out = append(out, headers[i], headers[i+1])
	}
	return out
}

// replaceHeader overrides one key's value in an alternating header list,
// appending it when absent.
func replaceHeader(headers []string, name, value string) []string {
	out := append(withoutHeader(headers, name), name, value)
	return out
}

// serve runs the full HTTP handler (middleware included) over a body and
// headers, returning the recorder.
func serve(t *testing.T, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	srv := newTestServer(t)
	c, w := postContext(t, body, headers...)
	if err := Handler(srv)(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return w
}

// headerError decodes the JSON-RPC error a 400 response carries.
func headerError(t *testing.T, w *httptest.ResponseRecorder) jsonrpc.Response {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	return decodeResponse(t, w.Body.Bytes())
}

// TestHeadersMirroringTheBodyAreAccepted asserts a compliant modern request is
// served normally: the guard must not stand in the way of correct clients.
func TestHeadersMirroringTheBodyAreAccepted(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"tools/call", modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)},
		{"tools/list", modernBody(1, "tools/list")},
		{"prompts/get", modernBody(1, "prompts/get", `"name":"review"`)},
		{"resources/read", modernBody(1, "resources/read", `"uri":"file://a.txt"`)},
		{"server/discover", modernBody(1, "server/discover")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body, mcpHeaders(t, tt.body)...)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestMissingRequiredHeaderIsRejected asserts each mandatory header is actually
// mandatory, and that the refusal names the header that is missing.
func TestMissingRequiredHeaderIsRejected(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)
	for _, name := range []string{HeaderProtocolVersion, HeaderMethod, HeaderName} {
		t.Run(name, func(t *testing.T) {
			w := serve(t, body, withoutHeader(mcpHeaders(t, body), name)...)
			resp := headerError(t, w)
			if resp.Error.Code != -32020 {
				t.Fatalf("code = %d, want -32020", resp.Error.Code)
			}
			want := "Header mismatch: The [" + name + "] header is required."
			if resp.Error.Message != want {
				t.Fatalf("message = %q, want %q", resp.Error.Message, want)
			}
			if string(resp.ID.Raw()) != "1" {
				t.Fatalf("id = %s, want 1", resp.ID.Raw())
			}
		})
	}
}

// TestContradictingHeaderIsRejected asserts a header that disagrees with the
// body is refused and that the message shows both values, so the client can see
// which of the two it got wrong.
func TestContradictingHeaderIsRejected(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		header  string
		value   string
		wantMsg string
	}{
		{
			name: "name", body: modernBody(1, "tools/call", `"name":"add","arguments":{}`),
			header: HeaderName, value: "some-other-tool",
			wantMsg: "Header mismatch: The [Mcp-Name] header value [some-other-tool] does not match the request body value [add].",
		},
		{
			name: "method", body: modernBody(1, "tools/list"),
			header: HeaderMethod, value: "tools/call",
			wantMsg: "Header mismatch: The [Mcp-Method] header value [tools/call] does not match the request body value [tools/list].",
		},
		{
			name: "protocol version", body: modernBody(1, "tools/list"),
			header: HeaderProtocolVersion, value: "2025-11-25",
			wantMsg: "Header mismatch: The [MCP-Protocol-Version] header value [2025-11-25] does not match the request body value [2026-07-28].",
		},
		{
			name: "uri for resources/read", body: modernBody(1, "resources/read", `"uri":"file://a.txt"`),
			header: HeaderName, value: "file://b.txt",
			wantMsg: "Header mismatch: The [Mcp-Name] header value [file://b.txt] does not match the request body value [file://a.txt].",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := replaceHeader(mcpHeaders(t, tt.body), tt.header, tt.value)
			resp := headerError(t, serve(t, tt.body, headers...))
			if resp.Error.Code != -32020 {
				t.Fatalf("code = %d, want -32020", resp.Error.Code)
			}
			if resp.Error.Message != tt.wantMsg {
				t.Fatalf("message = %q, want %q", resp.Error.Message, tt.wantMsg)
			}
		})
	}
}

// TestNameHeaderIsDecodedBeforeComparing asserts a wrapped name header is
// compared against the value it carries, not against its encoded form.
func TestNameHeaderIsDecodedBeforeComparing(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)
	headers := replaceHeader(mcpHeaders(t, body), HeaderName, "=?base64?YWRk?=")
	if w := serve(t, body, headers...); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

// TestOnlyTheNameHeaderIsDecoded asserts the wrapper is not honoured on the
// other headers: those carry values that are always header-safe, so accepting a
// wrapper there would be a second spelling for the same header.
func TestOnlyTheNameHeaderIsDecoded(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":"add","arguments":{}`)
	for _, name := range []string{HeaderMethod, HeaderProtocolVersion} {
		t.Run(name, func(t *testing.T) {
			// A wrapper carrying exactly the body value: honouring it would
			// make the header match, so a refusal proves it is not decoded.
			value := "=?base64?dG9vbHMvY2FsbA==?=" // "tools/call"
			if name == HeaderProtocolVersion {
				value = "=?base64?MjAyNi0wNy0yOA==?=" // "2026-07-28"
			}
			resp := headerError(t, serve(t, body, replaceHeader(mcpHeaders(t, body), name, value)...))
			if resp.Error.Code != -32020 {
				t.Fatalf("code = %d, want -32020", resp.Error.Code)
			}
		})
	}
}

// TestNameHeaderRequiredEvenWhenTheBodyHasNoUsableName asserts the header stays
// mandatory for a named method whose body names nothing usable, so a proxy can
// always route on it.
func TestNameHeaderRequiredEvenWhenTheBodyHasNoUsableName(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":123`)
	resp := headerError(t, serve(t, body, withoutHeader(mcpHeaders(t, body), HeaderName)...))
	if resp.Error.Message != "Header mismatch: The [Mcp-Name] header is required." {
		t.Fatalf("message = %q", resp.Error.Message)
	}
}

// TestNameHeaderAcceptedWhenTheBodyNamesNothing asserts that once the header is
// present, a body with no usable name is left to the method handler to report:
// the header machinery has nothing to compare against and must not guess.
func TestNameHeaderAcceptedWhenTheBodyNamesNothing(t *testing.T) {
	body := modernBody(1, "tools/call", `"name":123`)
	headers := append(mcpHeaders(t, body), HeaderName, "add")
	w := serve(t, body, headers...)
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected the method handler to report the bad name, got %+v", resp)
	}
	if resp.Error.Message != "Missing [name] parameter." {
		t.Fatalf("message = %q, want the method handler's own", resp.Error.Message)
	}
}

// TestNameHeaderNotRequiredForUnnamedMethods asserts a method that addresses no
// primitive needs no name header, and that supplying one anyway is harmless.
func TestNameHeaderNotRequiredForUnnamedMethods(t *testing.T) {
	body := modernBody(1, "tools/list")
	if got := mcpHeaders(t, body); len(got) != 4 {
		t.Fatalf("headers = %v, want no name header", got)
	}
	w := serve(t, body, append(mcpHeaders(t, body), HeaderName, "anything")...)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
}

// TestLegacyRequestsSkipHeaderValidation asserts a client that predates the
// headers keeps working over HTTP without sending any of them.
func TestLegacyRequestsSkipHeaderValidation(t *testing.T) {
	tests := []struct{ name, body string }{
		{"tools/list", `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`},
		{"initialize", string(initializeRequest(1))},
		{"tools/call", string(callToolRequest(2, 1, 2))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if resp := decodeResponse(t, w.Body.Bytes()); resp.Error != nil {
				t.Fatalf("legacy request refused: %+v", resp.Error)
			}
		})
	}
}

// TestNonRequestsSkipHeaderValidation asserts the guard stays out of the way of
// anything it cannot correlate to a request id, leaving the server to produce
// the proper JSON-RPC answer.
func TestNonRequestsSkipHeaderValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted},
		{"malformed json", `{"jsonrpc":"2.0",`, http.StatusOK},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, http.StatusAccepted},
		{"empty body", ``, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := serve(t, tt.body); w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantCode, w.Body.String())
			}
		})
	}
}

// TestHeaderMismatchEchoesTheRequestID asserts the refusal is correlated to the
// call, in the id's own JSON form, for every id type JSON-RPC allows a response
// to carry.
func TestHeaderMismatchEchoesTheRequestID(t *testing.T) {
	tests := []struct{ name, id, want string }{
		{"integer", `7`, `7`},
		{"string", `"abc"`, `"abc"`},
		{"negative integer", `-3`, `-3`},
		{"zero", `0`, `0`},
		{"unicode string", `"café ☕"`, `"café ☕"`},
		{"large integer beyond float precision", `9007199254740993`, `9007199254740993`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := modernIDBody(tt.id)
			resp := headerError(t, serve(t, body))
			if got := string(resp.ID.Raw()); got != tt.want {
				t.Fatalf("id = %s, want %s", got, tt.want)
			}
		})
	}
}

// modernIDBody builds a discovery-handshake tools/list body with a verbatim id
// token, so a test can state an id JSON-RPC does not permit.
func modernIDBody(id string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
}

// TestUnusableRequestIDIsNotEchoed asserts a body whose id is of a type
// JSON-RPC forbids in a response is not answered by the header guard at all.
// Such a message is not a request the server can correlate a reply to, so it is
// left to the server, which reports the invalid id itself with a null id, the
// same answer it gives on every other transport. Echoing the token back would
// put an object or an array where the specification allows only a string, a
// number or null.
func TestUnusableRequestIDIsNotEchoed(t *testing.T) {
	for _, id := range []string{`{}`, `{"a":1}`, `[1]`, `[]`, `true`, `false`} {
		t.Run(id, func(t *testing.T) {
			w := serve(t, modernIDBody(id))

			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error == nil {
				t.Fatalf("want an error for an unusable id, got %s", w.Body.String())
			}
			if resp.Error.Code != jsonrpc.CodeInvalidRequest {
				t.Fatalf("code = %d, want %d (body %s)", resp.Error.Code, jsonrpc.CodeInvalidRequest, w.Body.String())
			}
			if got := string(resp.ID.Raw()); got != "null" {
				t.Fatalf("id = %s, want null", got)
			}
		})
	}
}

// TestHeaderMismatchResponseShape pins the exact wire form of a refusal: status,
// content type and body.
func TestHeaderMismatchResponseShape(t *testing.T) {
	w := serve(t, modernBody(1, "tools/list"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	const want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"Header mismatch: The [MCP-Protocol-Version] header is required."}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
}

// TestProtocolVersionHeaderRequiredWhenTheBodyOmitsIt asserts the header duty is
// driven by the request being modern, not by the body happening to state a
// version: a request declaring only the client capabilities is modern too.
func TestProtocolVersionHeaderRequiredWhenTheBodyOmitsIt(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}`
	resp := headerError(t, serve(t, body, HeaderMethod, "tools/list"))
	if resp.Error.Message != "Header mismatch: The [MCP-Protocol-Version] header is required." {
		t.Fatalf("message = %q", resp.Error.Message)
	}
}

// TestHeaderValidationPrecedesHandling asserts the body never reaches a handler
// once a header fails: a refused request must have no side effect.
func TestHeaderValidationPrecedesHandling(t *testing.T) {
	var mu sync.Mutex
	handled := 0
	next := func(c *router.Context) error {
		mu.Lock()
		handled++
		mu.Unlock()
		return nil
	}
	c, w := postContext(t, modernBody(1, "tools/call", `"name":"add","arguments":{}`))
	if err := ValidateHeaders()(next)(c); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	if handled != 0 {
		t.Fatal("the handler ran despite a header failure")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestBodyIsHandedOnIntact asserts the middleware reads the body without
// consuming it: the handler behind it must see the same bytes.
func TestBodyIsHandedOnIntact(t *testing.T) {
	body := modernBody(1, "tools/list")
	var seen string
	next := func(c *router.Context) error {
		raw, err := readBody(c, DefaultMaxBodyBytes)
		if err != nil {
			return err
		}
		seen = string(raw)
		return nil
	}
	c, _ := postContext(t, body, mcpHeaders(t, body)...)
	if err := ValidateHeaders()(next)(c); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	if seen != body {
		t.Fatalf("handler saw %q, want %q", seen, body)
	}
}

// TestOversizedBodyIsRejectedByTheMiddleware asserts the body cap applies to the
// validation read as well, so the guard cannot become a way to feed the server
// an unbounded body.
func TestOversizedBodyIsRejectedByTheMiddleware(t *testing.T) {
	huge := modernBody(1, "tools/call", `"name":"`+strings.Repeat("a", 4096)+`"`)
	next := func(c *router.Context) error {
		t.Fatal("the handler ran despite an oversized body")
		return nil
	}
	c, w := postContext(t, huge)
	if err := ValidateHeaders(WithMaxBodyBytes(64))(next)(c); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeParseError {
		t.Fatalf("expected a parse error, got %+v", resp)
	}
	if strings.Contains(resp.Error.Message, "too large") {
		t.Fatalf("the refusal leaks the internal cause: %q", resp.Error.Message)
	}
}

// TestSubscriptionListenStreamsOverSSE asserts a subscription opened over HTTP
// receives both frames, the acknowledgement and the close, when the client
// negotiated an event stream.
func TestSubscriptionListenStreamsOverSSE(t *testing.T) {
	body := modernBody(7, "subscriptions/listen")
	w := serve(t, body, append(mcpHeaders(t, body), "Accept", "text/event-stream")...)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an event stream", ct)
	}
	payloads := sseFrames(w.Body.String())
	if len(payloads) != 2 {
		t.Fatalf("expected an acknowledgement and a result, got %d frames: %s", len(payloads), w.Body.String())
	}
	frames := make([]map[string]any, 0, len(payloads))
	for _, payload := range payloads {
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("decode sse frame %q: %v", payload, err)
		}
		frames = append(frames, frame)
	}
	if frames[0]["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("first frame = %v", frames[0])
	}
	if _, ok := frames[1]["result"]; !ok {
		t.Fatalf("last frame is not a result: %v", frames[1])
	}
}
