package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity-mcp/server"
)

// mcpHTTPServer is a minimal MCP server over HTTP for WebClient round-trip tests.
func mcpHTTPServer(t *testing.T, onAuth func(string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onAuth != nil {
			onAuth(r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &req)

		if len(req.ID) == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": server.LatestProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "http-fake", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "remote-tool"}}}
		default:
			result = map[string]any{}
		}

		w.Header().Set(sessionHeader, "sess-http")
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWebClientRoundTrip(t *testing.T) {
	srv := mcpHTTPServer(t, nil)
	w := Web(srv.URL)

	tools, err := w.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "remote-tool" {
		t.Fatalf("tools = %+v", tools)
	}
	if w.InitializeResult().ServerInfo.Name != "http-fake" {
		t.Fatalf("init result = %+v", w.InitializeResult())
	}
	w.Disconnect()
}

func TestWebClientWithToken(t *testing.T) {
	var auth string
	srv := mcpHTTPServer(t, func(a string) {
		if a != "" {
			auth = a
		}
	})
	w := Web(srv.URL).WithToken("tok-123")

	if err := w.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if auth != "Bearer tok-123" {
		t.Fatalf("authorization header = %q", auth)
	}
}

func TestWebClientOAuthClient(t *testing.T) {
	w := Web("https://example.com/mcp")

	if _, err := w.OAuthClient("", ""); err == nil {
		t.Fatal("expected error before WithOAuth")
	}

	w.WithOAuth(oauth.Config{ClientID: "cid", RedirectURI: "http://localhost/cb"})
	oc, err := w.OAuthClient("https://example.com/.well-known/oauth-protected-resource", "mcp:use")
	if err != nil {
		t.Fatalf("oauth client: %v", err)
	}
	if oc == nil {
		t.Fatal("nil oauth client")
	}
}
