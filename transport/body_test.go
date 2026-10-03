package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity/contract"
	"github.com/velocitykode/velocity/router"
)

// This file covers what a client is told when its request body cannot be taken
// in at all. A body over the cap is 413 (RFC 9110 section 15.5.14) and one that
// cannot be read is 400, both rendered by the framework's error boundary from
// the error the handler returns. Neither is a JSON-RPC parse error: the body was
// never parsed, so it was not invalid JSON, and a client told otherwise would
// resend the same bytes.

// TestOversizedBodyIsAnswered413ThroughTheRouter asserts the refusal reaches a
// client as HTTP 413 when the handler is mounted on the framework router, with
// the server never called, and that a body within the cap on the same route is
// served.
func TestOversizedBodyIsAnswered413ThroughTheRouter(t *testing.T) {
	stub := &stubServer{fn: func(context.Context, []byte, string) server.HandleResult {
		return server.HandleResult{HasResponse: false}
	}}
	r := router.NewV2()
	r.Post("/mcp", Handler(stub, WithMaxBodyBytes(64)))
	ts := httptest.NewServer(r)
	defer ts.Close()

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCalls  int
	}{
		{"over the cap", `{"jsonrpc":"2.0","method":"ping","params":{"pad":"` + strings.Repeat("x", 128) + `"}}`, http.StatusRequestEntityTooLarge, 0},
		{"within the cap", `{"jsonrpc":"2.0","method":"ping"}`, http.StatusAccepted, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := stub.callCount()
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", contentTypeJSON)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tt.wantStatus, body)
			}
			if strings.Contains(string(body), "-32700") {
				t.Fatalf("a body the server never parsed was reported as invalid JSON: %s", body)
			}
			if got := stub.callCount() - before; got != tt.wantCalls {
				t.Fatalf("server called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}

// TestUnreadableBodyIsAnswered400WithoutItsCause asserts a body the connection
// fails to deliver is a 400 carrying a fixed message, with the cause attached
// for the log and kept out of the message, and that the server is never called.
func TestUnreadableBodyIsAnswered400WithoutItsCause(t *testing.T) {
	cause := errors.New("internal: socket detail that must not reach the client")
	stub := &stubServer{fn: func(context.Context, []byte, string) server.HandleResult {
		t.Error("the server was called for a body that could not be read")
		return server.HandleResult{}
	}}

	c, w := router.NewTestContext(http.MethodPost, "/mcp", iotest.ErrReader(cause))
	c.Request.Header.Set("Content-Type", contentTypeJSON)
	c.Request.ContentLength = -1
	err := Handler(stub)(c)

	var httpErr *contract.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("handler returned %v, want a *contract.HTTPError", err)
	}
	if httpErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", httpErr.Status)
	}
	if httpErr.Message != "The request body could not be read." {
		t.Fatalf("message = %q, want the fixed text", httpErr.Message)
	}
	if strings.Contains(httpErr.Message, "socket") {
		t.Fatalf("the cause leaked into the message: %q", httpErr.Message)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("the cause is not attached for the log: %v", err)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("handler wrote %q, want nothing", w.Body.String())
	}
	if stub.callCount() != 0 {
		t.Fatalf("server called %d times, want 0", stub.callCount())
	}
}
