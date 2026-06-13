package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/router"
)

// postContext builds a router.Context for a POST to /mcp with the given body and
// optional headers, returning the context and its recorder. Headers are supplied
// as alternating key/value pairs.
func postContext(t *testing.T, body string, headers ...string) (*router.Context, *httptest.ResponseRecorder) {
	t.Helper()
	if len(headers)%2 != 0 {
		t.Fatalf("headers must be key/value pairs, got %d", len(headers))
	}
	c, w := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader(body))
	for i := 0; i < len(headers); i += 2 {
		c.Request.Header.Set(headers[i], headers[i+1])
	}
	return c, w
}

func TestHandler_Initialize(t *testing.T) {
	srv := newTestServer(t)
	h := Handler(srv)

	c, w := postContext(t, string(initializeRequest(1)))
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Fatalf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}
	if sid := w.Header().Get(sessionHeader); sid != "fixed-session" {
		t.Fatalf("%s = %q, want fixed-session", sessionHeader, sid)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %+v", resp.Error)
	}
}

func TestHandler_ToolCall(t *testing.T) {
	srv := newTestServer(t)
	h := Handler(srv)

	c, w := postContext(t, string(callToolRequest(2, 1, 2)))
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// No session id is assigned for a non-initialize request.
	if sid := w.Header().Get(sessionHeader); sid != "" {
		t.Fatalf("%s = %q, want empty", sessionHeader, sid)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("tools/call returned error: %+v", resp.Error)
	}
	if !bytes.Contains(resp.Result, []byte(`"3"`)) {
		t.Fatalf("result %s does not contain \"3\"", resp.Result)
	}
}

func TestHandler_Notification_Returns202(t *testing.T) {
	srv := newTestServer(t)
	h := Handler(srv)

	// A message without an id is a notification: no reply, HTTP 202.
	c, w := postContext(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

func TestHandler_BadJSON_ParseError(t *testing.T) {
	srv := newTestServer(t)
	h := Handler(srv)

	c, w := postContext(t, `{not json`)
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil {
		t.Fatalf("want JSON-RPC error for bad JSON, got %s", w.Body.Bytes())
	}
	if resp.Error.Code != jsonrpc.CodeParseError {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeParseError)
	}
}

func TestHandler_OversizedBody_Rejected(t *testing.T) {
	srv := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		t.Errorf("server.Handle should not be called for an oversized body")
		return server.HandleResult{}
	}}
	h := Handler(srv, WithMaxBodyBytes(16))

	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 1024) + `"}}`
	c, w := postContext(t, big)
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeParseError {
		t.Fatalf("want parse error for oversized body, got %s", w.Body.Bytes())
	}
	if srv.callCount() != 0 {
		t.Fatalf("server was called %d times, want 0", srv.callCount())
	}
}

func TestHandler_WithMaxBodyBytes_NonPositiveIgnored(t *testing.T) {
	// A non-positive override must be ignored so the body is always bounded.
	o := httpOptions{maxBodyBytes: DefaultMaxBodyBytes}
	WithMaxBodyBytes(0)(&o)
	WithMaxBodyBytes(-5)(&o)
	if o.maxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("maxBodyBytes = %d, want default %d", o.maxBodyBytes, DefaultMaxBodyBytes)
	}
	WithMaxBodyBytes(99)(&o)
	if o.maxBodyBytes != 99 {
		t.Fatalf("maxBodyBytes = %d, want 99", o.maxBodyBytes)
	}
}

func TestHandler_SessionHeaderFlow(t *testing.T) {
	// The inbound session header is forwarded to the server as the existing
	// session id; an initialize assigns a new one echoed back in the header.
	var seen []string
	var mu sync.Mutex
	srv := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		mu.Lock()
		seen = append(seen, sessionID)
		mu.Unlock()
		if strings.Contains(string(raw), "initialize") {
			return server.HandleResult{
				Response:    mustResult(t, jsonrpc.IntID(1), map[string]any{"ok": true}),
				HasResponse: true,
				SessionID:   "new-session",
			}
		}
		return server.HandleResult{
			Response:    mustResult(t, jsonrpc.IntID(2), map[string]any{"pong": true}),
			HasResponse: true,
		}
	}}
	h := Handler(srv)

	// 1. initialize with no inbound session -> server sees "" -> assigns one.
	c, w := postContext(t, string(initializeRequest(1)))
	if err := h(c); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := w.Header().Get(sessionHeader); got != "new-session" {
		t.Fatalf("response %s = %q, want new-session", sessionHeader, got)
	}

	// 2. follow-up request carrying the session header -> server sees it.
	c2, _ := postContext(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, sessionHeader, "  new-session  ")
	if err := h(c2); err != nil {
		t.Fatalf("ping: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server called %d times, want 2", len(seen))
	}
	if seen[0] != "" {
		t.Fatalf("first call session = %q, want empty", seen[0])
	}
	if seen[1] != "new-session" {
		t.Fatalf("second call session = %q, want new-session (trimmed)", seen[1])
	}
}

func TestHandler_SSEResponseMode(t *testing.T) {
	srv := newTestServer(t)
	h := Handler(srv)

	// A client accepting only text/event-stream gets an SSE-framed reply.
	c, w := postContext(t, string(initializeRequest(1)), "Accept", "text/event-stream")
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("SSE body must start with 'data: ', got %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("SSE body must end with blank line, got %q", body)
	}
	// The session id is still surfaced in the header in SSE mode.
	if sid := w.Header().Get(sessionHeader); sid != "fixed-session" {
		t.Fatalf("%s = %q, want fixed-session", sessionHeader, sid)
	}
	// The payload between the framing must be a valid JSON-RPC response.
	payload := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	resp := decodeResponse(t, []byte(payload))
	if resp.Error != nil {
		t.Fatalf("SSE payload carried error: %+v", resp.Error)
	}
}

func TestWantsEventStream(t *testing.T) {
	tests := []struct {
		name   string
		accept string
		want   bool
	}{
		{"empty header", "", false},
		{"json only", "application/json", false},
		{"sse only", "text/event-stream", true},
		{"sse with params", "text/event-stream; charset=utf-8", true},
		{"both json first", "application/json, text/event-stream", false},
		{"both sse first", "text/event-stream, application/json", false},
		{"wildcard accept", "text/event-stream, */*", false},
		{"application wildcard", "text/event-stream, application/*", false},
		{"unrelated", "text/html", false},
		{"sse uppercase", "TEXT/EVENT-STREAM", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader("{}"))
			if tt.accept != "" {
				c.Request.Header.Set("Accept", tt.accept)
			}
			if got := wantsEventStream(c); got != tt.want {
				t.Fatalf("wantsEventStream(%q) = %v, want %v", tt.accept, got, tt.want)
			}
		})
	}
}

// progressServer builds a server whose "work" tool reports two progress steps
// before returning, for exercising the streaming path.
func progressServer(t *testing.T) *server.Server {
	t.Helper()
	work := server.NewTool("work", "does work in steps").
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			_ = req.ReportProgress(server.ProgressUpdate{Progress: 1, Total: 2, Message: "half"})
			_ = req.ReportProgress(server.ProgressUpdate{Progress: 2, Total: 2})
			return server.Text("done"), nil
		})
	srv := server.New("test", "1.0.0", server.WithTools(work))
	srv.SetSessionIDGenerator(func() string { return "fixed-session" })
	return srv
}

// sseFrames splits an SSE body into the JSON payloads of its "data:" frames.
func sseFrames(body string) []string {
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		out = append(out, strings.TrimPrefix(frame, "data: "))
	}
	return out
}

func TestHandler_StreamingProgress(t *testing.T) {
	h := Handler(progressServer(t))

	// A tools/call carrying a progressToken, with an event-stream Accept, streams
	// progress notifications followed by the final result on one SSE response.
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"work","arguments":{},"_meta":{"progressToken":"tok-1"}}}`
	c, w := postContext(t, body, "Accept", "text/event-stream")
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frames := sseFrames(w.Body.String())
	if len(frames) != 3 {
		t.Fatalf("want 3 SSE frames (2 progress + result), got %d: %q", len(frames), frames)
	}

	// First two frames are notifications/progress for the supplied token.
	for i, f := range frames[:2] {
		var n struct {
			Method string `json:"method"`
			Params struct {
				ProgressToken string  `json:"progressToken"`
				Progress      float64 `json:"progress"`
				Total         float64 `json:"total"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(f), &n); err != nil {
			t.Fatalf("frame %d decode: %v (%q)", i, err, f)
		}
		if n.Method != "notifications/progress" || n.Params.ProgressToken != "tok-1" {
			t.Fatalf("frame %d = %q, want progress for tok-1", i, f)
		}
		if n.Params.Progress != float64(i+1) || n.Params.Total != 2 {
			t.Fatalf("frame %d progress = %v/%v", i, n.Params.Progress, n.Params.Total)
		}
	}

	// Final frame is the tools/call result correlating to the request id.
	resp := decodeResponse(t, []byte(frames[2]))
	if resp.Error != nil {
		t.Fatalf("final frame carried error: %+v", resp.Error)
	}
	if resp.ID.String() != "7" {
		t.Fatalf("final frame id = %s, want 7", resp.ID.String())
	}
}

func TestHandler_NoProgressTokenStaysBuffered(t *testing.T) {
	h := Handler(progressServer(t))

	// Without a progressToken, an event-stream Accept still yields the buffered
	// single-frame SSE reply (no progress notifications): the tool's
	// ReportProgress calls are no-ops because no streaming sink is wired.
	body := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"work","arguments":{}}}`
	c, w := postContext(t, body, "Accept", "text/event-stream")
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	frames := sseFrames(w.Body.String())
	if len(frames) != 1 {
		t.Fatalf("want a single buffered frame, got %d: %q", len(frames), frames)
	}
	resp := decodeResponse(t, []byte(frames[0]))
	if resp.Error != nil || resp.ID.String() != "8" {
		t.Fatalf("buffered frame = %q", frames[0])
	}
}

func TestHandler_NilBody(t *testing.T) {
	// A request with a nil body must not panic; it drives an empty message
	// through the server which yields a parse error.
	srv := newTestServer(t)
	h := Handler(srv)

	c, w := router.NewTestContext(http.MethodPost, "/mcp")
	c.Request.Body = nil
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil {
		t.Fatalf("want a JSON-RPC error for empty body, got %s", w.Body.Bytes())
	}
}

func TestHandler_MarshalFailure_InternalError(t *testing.T) {
	// A response whose Result is invalid JSON cannot be re-marshalled; the
	// handler must emit a generic JSON-RPC internal error, not leak detail.
	srv := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{
			Response: &jsonrpc.Response{
				JSONRPC: "2.0",
				ID:      jsonrpc.IntID(7),
				Result:  []byte(`{invalid`), // not valid JSON -> Marshal fails
			},
			HasResponse: true,
		}
	}}
	h := Handler(srv)

	c, w := postContext(t, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInternalError {
		t.Fatalf("want internal error, got %s", w.Body.Bytes())
	}
	if strings.Contains(strings.ToLower(resp.Error.Message), "invalid") {
		t.Fatalf("internal error message leaked detail: %q", resp.Error.Message)
	}
	// The id of the original response is preserved.
	if got, want := resp.ID.String(), jsonrpc.IntID(7).String(); got != want {
		t.Fatalf("error response id = %s, want %s", got, want)
	}
}

// capturingLogger records Error calls so the logf server-side reporting path can
// be asserted. It satisfies the subset of contract.Logger that logf uses.
type capturingLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *capturingLogger) Debug(msg string, kvs ...any) {}
func (l *capturingLogger) Info(msg string, kvs ...any)  {}
func (l *capturingLogger) Warn(msg string, kvs ...any)  {}
func (l *capturingLogger) Error(msg string, kvs ...any) {
	l.mu.Lock()
	l.msgs = append(l.msgs, msg)
	l.mu.Unlock()
}
func (l *capturingLogger) Fatal(msg string, kvs ...any) {}

func (l *capturingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.msgs)
}

func TestHandler_OversizedBody_LogsServerSide(t *testing.T) {
	srv := newTestServer(t)
	logger := &capturingLogger{}
	h := Handler(srv, WithMaxBodyBytes(8))

	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 512) + `"}}`
	c, w := postContext(t, big)
	c.SetServices(&app.Services{Log: logger})

	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if logger.count() == 0 {
		t.Fatalf("expected the oversized-body cause to be logged server-side")
	}
}

func TestLogf_NoServicesNoPanic(t *testing.T) {
	// logf must degrade to a no-op when services are not wired and on a nil err.
	c, _ := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader("{}"))
	logf(c, nil)                      // nil error: no-op
	logf(c, errors.New("boom"))       // no services wired: no-op, no panic
	c.SetServices(&app.Services{})    // services present but Log nil
	logf(c, errors.New("still boom")) // no-op, no panic
}

func TestMaxBytesError(t *testing.T) {
	if maxBytesError(errors.New("plain")) {
		t.Fatal("plain error misclassified as MaxBytesError")
	}
	if !maxBytesError(&http.MaxBytesError{Limit: 1}) {
		t.Fatal("MaxBytesError not recognised")
	}
}

// TestHandler_EndToEndViaRouter mounts the handler on a real velocity router and
// drives a request through ServeHTTP, exercising the full mount path apps use.
func TestHandler_EndToEndViaRouter(t *testing.T) {
	srv := newTestServer(t)
	r := router.New()
	r.Post("/mcp", Handler(srv))

	ts := httptest.NewServer(r)
	defer ts.Close()

	// initialize
	resp, err := http.Post(ts.URL+"/mcp", contentTypeJSON, bytes.NewReader(initializeRequest(1)))
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	sid := resp.Header.Get(sessionHeader)
	if sid == "" {
		t.Fatalf("no %s header on initialize response", sessionHeader)
	}
	body, _ := io.ReadAll(resp.Body)
	ir := decodeResponse(t, body)
	if ir.Error != nil {
		t.Fatalf("initialize error: %+v", ir.Error)
	}

	// tools/call carrying the session id
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(callToolRequest(2, 2, 5)))
	req.Header.Set("Content-Type", contentTypeJSON)
	req.Header.Set(sessionHeader, sid)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST tools/call: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	cr := decodeResponse(t, body2)
	if cr.Error != nil {
		t.Fatalf("tools/call error: %+v", cr.Error)
	}
	if !bytes.Contains(cr.Result, []byte(`"7"`)) {
		t.Fatalf("tools/call result %s does not contain \"7\"", cr.Result)
	}
}

func TestHandler_Concurrent(t *testing.T) {
	srv := newTestServer(t)
	h := Handler(srv)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c, w := postContext(t, string(callToolRequest(int64(i), float64(i), 1)))
			if err := h(c); err != nil {
				errs <- err
				return
			}
			if w.Code != http.StatusOK {
				errs <- fmt.Errorf("status %d", w.Code)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent handler error: %v", err)
	}
}
