package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity/contract"
	"github.com/velocitykode/velocity/router"
)

// This file covers what a request must declare its body as. The streamable
// HTTP transport carries JSON-RPC and nothing else, so a body declared as
// anything but application/json, or declared as nothing, is refused with 415
// (RFC 9110 section 15.5.16) before it is read. The media types a browser sends
// cross-site without a preflight (the Fetch standard's CORS-safelisted values
// for Content-Type: text/plain, application/x-www-form-urlencoded,
// multipart/form-data) are exactly the ones this refuses, so a page on another
// origin cannot run a tool through a request the browser sends blind.

// mediaTypeRefusals are the declarations a request carrying a body may not
// make.
var mediaTypeRefusals = []struct {
	name        string
	contentType string
}{
	{"none", ""},
	{"text/plain", "text/plain"},
	{"text/plain with charset", "text/plain; charset=utf-8"},
	{"form encoding", "application/x-www-form-urlencoded"},
	{"multipart", "multipart/form-data; boundary=x"},
	{"octet stream", "application/octet-stream"},
	{"a json suffix type", "application/json-rpc"},
	{"a structured syntax suffix", "application/problem+json"},
	{"text/json", "text/json"},
}

// refusedMediaType runs the handler over a tools/call declared with the given
// Content-Type and returns the error it refused with. A declaration of "" sends
// no header at all.
func refusedMediaType(t *testing.T, srv MCPServer, contentType string) error {
	t.Helper()
	c, _ := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader(legacyToolCall))
	if contentType != "" {
		c.Request.Header.Set("Content-Type", contentType)
	}
	return Handler(srv)(c)
}

// TestABodyNotDeclaredAsJSONIsRefusedWith415 asserts every other declaration is
// answered with the framework's 415 and never reaches the server.
func TestABodyNotDeclaredAsJSONIsRefusedWith415(t *testing.T) {
	for _, tt := range mediaTypeRefusals {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubServer{fn: func(context.Context, []byte, string) server.HandleResult {
				t.Error("the server was called for a body not declared as JSON")
				return server.HandleResult{}
			}}
			err := refusedMediaType(t, stub, tt.contentType)
			var httpErr *contract.HTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("handler returned %v, want a *contract.HTTPError", err)
			}
			if httpErr.Status != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415", httpErr.Status)
			}
			if stub.callCount() != 0 {
				t.Fatalf("server called %d times, want 0", stub.callCount())
			}
		})
	}
}

// TestAJSONDeclarationInAnyFormIsServed asserts the declaration is matched as a
// media type: parameters and letter case do not matter.
func TestAJSONDeclarationInAnyFormIsServed(t *testing.T) {
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON", "Application/Json;charset=UTF-8"} {
		t.Run(contentType, func(t *testing.T) {
			c, w := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader(legacyToolCall))
			c.Request.Header.Set("Content-Type", contentType)
			if err := Handler(newTestServer(t))(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"3"`) {
				t.Fatalf("the tool did not run: %s", w.Body.String())
			}
		})
	}
}

// TestANotificationNotDeclaredAsJSONIsRefusedToo asserts the declaration is
// checked before the message is classified: a notification, which would
// otherwise be acknowledged without a reply, is refused like any request.
func TestANotificationNotDeclaredAsJSONIsRefusedToo(t *testing.T) {
	for _, tt := range mediaTypeRefusals {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubServer{fn: func(context.Context, []byte, string) server.HandleResult {
				return server.HandleResult{}
			}}
			c, _ := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
			if tt.contentType != "" {
				c.Request.Header.Set("Content-Type", tt.contentType)
			}
			err := Handler(stub)(c)
			var httpErr *contract.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnsupportedMediaType {
				t.Fatalf("handler returned %v, want a 415", err)
			}
			if stub.callCount() != 0 {
				t.Fatalf("server called %d times, want 0", stub.callCount())
			}
		})
	}
}

// TestValidateHeadersOnItsOwnRefusesABodyNotDeclaredAsJSON asserts the
// middleware mounted without Handler applies the same rule, before it reads the
// body or runs the handler behind it.
func TestValidateHeadersOnItsOwnRefusesABodyNotDeclaredAsJSON(t *testing.T) {
	for _, tt := range mediaTypeRefusals {
		t.Run(tt.name, func(t *testing.T) {
			next := func(c *router.Context) error {
				t.Fatal("the handler ran for a body not declared as JSON")
				return nil
			}
			c, _ := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader(legacyToolCall))
			if tt.contentType != "" {
				c.Request.Header.Set("Content-Type", tt.contentType)
			}
			err := ValidateHeaders()(next)(c)
			var httpErr *contract.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnsupportedMediaType {
				t.Fatalf("middleware returned %v, want a 415", err)
			}
		})
	}
}

// TestAnUndeclaredBodyIsRefusedThroughTheRouter asserts the refusal reaches a
// client as an HTTP 415 when the handler is mounted on the framework router,
// which is where the error the middleware returns is rendered, and that a
// declared body on the same route is served.
func TestAnUndeclaredBodyIsRefusedThroughTheRouter(t *testing.T) {
	stub := &stubServer{fn: func(context.Context, []byte, string) server.HandleResult {
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), map[string]any{"ok": true}), HasResponse: true}
	}}
	r := router.NewV2()
	r.Post("/mcp", Handler(stub))
	ts := httptest.NewServer(r)
	defer ts.Close()

	for _, tt := range []struct {
		name        string
		contentType string
		wantStatus  int
		wantCalls   int
	}{
		{"text/plain", "text/plain", http.StatusUnsupportedMediaType, 0},
		{"application/json", "application/json", http.StatusOK, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := stub.callCount()
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(legacyList))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tt.contentType)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tt.wantStatus, body)
			}
			if got := stub.callCount() - before; got != tt.wantCalls {
				t.Fatalf("server called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}
