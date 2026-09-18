package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// boomMethod fails the way a handler that hit an unexpected condition does: with
// a plain Go error, which the server turns into a generic internal error.
type boomMethod struct{}

func (boomMethod) Handle(*server.Context, *jsonrpc.Request) (*jsonrpc.Response, error) {
	return nil, context.DeadlineExceeded
}

// serveWith runs the full HTTP handler over a body and headers against a
// caller-supplied server.
func serveWith(t *testing.T, srv MCPServer, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	c, w := postContext(t, body, headers...)
	if err := Handler(srv)(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return w
}

// TestModernReplyCarriesTheMappedStatus asserts a discovery-handshake client
// learns the outcome from the HTTP status as well as from the JSON-RPC error
// object: a method the server does not implement is 404, a server-side failure
// is 500, every other refusal is 400, and a result is 200.
func TestModernReplyCarriesTheMappedStatus(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   int
	}{
		{
			name:       "result",
			body:       modernBody(1, "tools/list"),
			wantStatus: http.StatusOK,
		},
		{
			name:       "unsupported protocol version",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   jsonrpc.CodeUnsupportedProtocolVersion,
		},
		{
			name:       "missing protocol metadata member",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   jsonrpc.CodeInvalidParams,
		},
		{
			name:       "unknown method",
			body:       modernBody(1, "does/not/exist"),
			wantStatus: http.StatusNotFound,
			wantCode:   jsonrpc.CodeMethodNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := []string{HeaderProtocolVersion, protocolVersionOf(t, tt.body), HeaderMethod, methodOf(t, tt.body)}
			w := serve(t, tt.body, headers...)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if tt.wantCode == 0 {
				if resp.Error != nil {
					t.Fatalf("unexpected error: %+v", resp.Error)
				}
				return
			}
			if resp.Error == nil || resp.Error.Code != tt.wantCode {
				t.Fatalf("error = %+v, want code %d", resp.Error, tt.wantCode)
			}
		})
	}
}

// TestModernInternalFailureIs500 asserts a handler that fails for a reason the
// client cannot act on is reported as a server-side failure, with no internal
// detail in the body.
func TestModernInternalFailureIs500(t *testing.T) {
	srv := server.New("test", "1.0.0", server.WithMethod("custom/boom", boomMethod{}))
	body := modernBody(1, "custom/boom")
	w := serveWith(t, srv, body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "custom/boom")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError {
		t.Fatalf("error = %+v, want an internal error", resp.Error)
	}
	if strings.Contains(resp.Error.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("the reply leaks the internal cause: %q", resp.Error.Message)
	}
}

// TestLegacyReplyKeepsStatus200 asserts the mapping is confined to the
// discovery handshake. A client that predates it may treat any non-2xx reply as
// a transport failure and never read the body, so the JSON-RPC error object has
// to reach it under a 200.
func TestLegacyReplyKeepsStatus200(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"does/not/exist","params":{}}`, jsonrpc.CodeMethodNotFound},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, jsonrpc.CodeInvalidParams},
		{"invalid request", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, jsonrpc.CodeInvalidRequest},
		{"parse error", `{not json`, jsonrpc.CodeParseError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, tt.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			resp := decodeResponse(t, w.Body.Bytes())
			if resp.Error == nil || resp.Error.Code != tt.wantCode {
				t.Fatalf("error = %+v, want code %d", resp.Error, tt.wantCode)
			}
		})
	}
}

// TestMappedStatusReachesTheEventStreamFraming asserts the status travels with
// the reply whatever framing the client negotiated, so a client that accepts
// only an event stream is not told a refused request succeeded.
func TestMappedStatusReachesTheEventStreamFraming(t *testing.T) {
	body := modernBody(1, "does/not/exist")
	w := serve(t, body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "does/not/exist", "Accept", "text/event-stream")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an event stream", ct)
	}
}

// TestRefusedSubscriptionIsNotStreamed asserts a subscription the server
// refuses before the handler runs is answered with the ordinary buffered reply,
// not with an event stream. The stream framing is a commitment made by the
// first frame a handler produces; a request that never reached one has produced
// no frames, and committing anyway would put the refusal where a client is
// reading for notifications.
func TestRefusedSubscriptionIsNotStreamed(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`
	w := serve(t, body,
		HeaderProtocolVersion, "1999-01-01",
		HeaderMethod, "subscriptions/listen",
		"Accept", "application/json, text/event-stream",
	)

	if ct := w.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q (body %s)", ct, contentTypeJSON, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "data: ") {
		t.Fatalf("the refusal was framed as an event: %s", w.Body.String())
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeUnsupportedProtocolVersion {
		t.Fatalf("error = %+v, want the unsupported-version refusal", resp.Error)
	}
	if got := string(resp.ID.Raw()); got != "7" {
		t.Fatalf("id = %s, want 7", got)
	}
}

// TestRefusedSubscriptionKeepsTheEventStreamForAStreamOnlyClient asserts the
// fallback still honours the Accept header: a client that cannot read JSON gets
// its refusal as a single event, because that is the only framing it accepts.
func TestRefusedSubscriptionKeepsTheEventStreamForAStreamOnlyClient(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`
	w := serve(t, body,
		HeaderProtocolVersion, "1999-01-01",
		HeaderMethod, "subscriptions/listen",
		"Accept", "text/event-stream",
	)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an event stream", ct)
	}
	frames := sseFrames(w.Body.String())
	if len(frames) != 1 {
		t.Fatalf("want a single refusal frame, got %d: %q", len(frames), frames)
	}
	resp := decodeResponse(t, []byte(frames[0]))
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeUnsupportedProtocolVersion {
		t.Fatalf("error = %+v, want the unsupported-version refusal", resp.Error)
	}
}

// TestStreamPathNotificationIsAcknowledged asserts a message that takes the
// streaming path but produces no reply (a notification carrying a progress
// token) is acknowledged the same way as on the buffered path, rather than
// leaving the client an empty event stream to parse.
func TestStreamPathNotificationIsAcknowledged(t *testing.T) {
	const body = `{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"progressToken":"tok"}}}`
	w := serve(t, body, "Accept", "text/event-stream")

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty", got)
	}
}

// protocolVersionOf reads the protocol version a test body states, for building
// the header that mirrors it.
func protocolVersionOf(t *testing.T, body string) string {
	t.Helper()
	return metaMember(t, body, "io.modelcontextprotocol/protocolVersion")
}

// methodOf reads the method a test body states.
func methodOf(t *testing.T, body string) string {
	t.Helper()
	var msg struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return msg.Method
}

// metaMember reads a string member of a body's params._meta object.
func metaMember(t *testing.T, body, key string) string {
	t.Helper()
	var msg struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	value, _ := msg.Params.Meta[key].(string)
	return value
}
