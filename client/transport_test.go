package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

func TestStdioRoundTrip(t *testing.T) {
	// `cat` echoes each line, exercising the newline framing both ways.
	tr := NewStdioTransport("cat")
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	if err := tr.Send(context.Background(), `{"jsonrpc":"2.0","id":1}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := tr.Receive(context.Background())
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got != `{"jsonrpc":"2.0","id":1}` {
		t.Fatalf("round-trip = %q", got)
	}
}

func TestStdioMissingCommand(t *testing.T) {
	tr := NewStdioTransport("velocity-mcp-no-such-binary-zzz")
	err := tr.Connect(context.Background())
	if err == nil {
		tr.Disconnect()
		t.Fatal("expected error starting a missing command")
	}
}

func TestStdioReceiveTimeout(t *testing.T) {
	// `sleep` produces no output; Receive must time out rather than block.
	tr := NewStdioTransport("sleep", "5")
	tr.SetTimeout(100 * time.Millisecond)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	start := time.Now()
	if _, err := tr.Receive(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("receive blocked too long: %v", elapsed)
	}
}

func TestHTTPJSONResponseAndSession(t *testing.T) {
	var sawSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSession = r.Header.Get(sessionHeader)
		w.Header().Set(sessionHeader, "sess-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL)
	ctx := context.Background()

	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if msg, err := tr.Receive(ctx); err != nil || msg == "" {
		t.Fatalf("receive: %q err=%v", msg, err)
	}

	// The captured session id is echoed on the next request.
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if sawSession != "sess-1" {
		t.Fatalf("session id not echoed; saw %q", sawSession)
	}
}

func TestHTTPAuthorizationRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://as.example.com/.well-known/oauth-protected-resource", scope="mcp:use"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL)
	err := tr.Send(context.Background(), `{}`)
	var authErr *oauth.AuthorizationRequiredError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected AuthorizationRequiredError, got %v", err)
	}
	if authErr.ResourceMetadataURL() != "https://as.example.com/.well-known/oauth-protected-resource" {
		t.Fatalf("challenge metadata url = %q", authErr.ResourceMetadataURL())
	}
	if authErr.Scope() != "mcp:use" {
		t.Fatalf("challenge scope = %q", authErr.Scope())
	}
}

func TestHTTPSessionExpiry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set(sessionHeader, "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL)
	ctx := context.Background()
	if err := tr.Send(ctx, `{"id":1}`); err != nil {
		t.Fatalf("first send: %v", err)
	}
	_, _ = tr.Receive(ctx)

	err := tr.Send(ctx, `{"id":2}`)
	if !errors.Is(err, errSessionExpired) {
		t.Fatalf("expected session expiry, got %v", err)
	}
}

func TestHTTPServerEventStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL)
	ctx := context.Background()
	if err := tr.Send(ctx, `{"id":1}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if msg != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("sse frame = %q", msg)
	}
}

func TestHTTPServerStreamRequestRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A server-initiated request (method + id) over the stream is unsupported.
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"sampling/createMessage\"}\n\n"))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(srv.URL)
	if err := tr.Send(context.Background(), `{"id":1}`); err == nil {
		t.Fatal("expected error for server-initiated stream request")
	}
}
