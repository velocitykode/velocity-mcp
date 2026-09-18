package mcpclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// mountModule installs a module's routes on a fresh router, optionally with
// middleware declared on the web stack, and returns a function that drives one
// request through the router.
func mountModule(t *testing.T, p *OAuthRouteModule, webMiddleware ...router.MiddlewareFunc) func(method, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := router.NewV2()
	stack := chain.NewMiddlewareStack(&velapp.Services{})
	stack.Web(webMiddleware...)
	p.Routes(chain.NewRouting(r, stack))

	return func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
}

// decodeDocument parses a client ID metadata document response.
func decodeDocument(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("document is not JSON: %v; body: %s", err, rec.Body.String())
	}
	return doc
}

// wantStrings asserts a decoded JSON array equals the expected strings.
func wantStrings(t *testing.T, value any, want []string) {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("value %#v is not a JSON array", value)
	}
	if len(items) != len(want) {
		t.Fatalf("value = %#v, want %v", items, want)
	}
	for i, item := range items {
		if s, ok := item.(string); !ok || s != want[i] {
			t.Fatalf("value[%d] = %#v, want %q", i, item, want[i])
		}
	}
}

func TestClientMetadataDocumentIsServedOverTheWire(t *testing.T) {
	p := OAuthRoutesFor("github", oauth.Config{}, WithPublicURL("https://app.example.com/"))
	call := mountModule(t, p)

	rec := call(http.MethodGet, "http://spoofed.example.net/mcp/oauth/github/client-metadata.json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control = %q, want public, max-age=3600", got)
	}

	doc := decodeDocument(t, rec)
	// The document is built from the configured public URL, never from the
	// (here spoofed) request host.
	if got := doc["client_id"]; got != "https://app.example.com/mcp/oauth/github/client-metadata.json" {
		t.Fatalf("client_id = %#v", got)
	}
	wantStrings(t, doc["redirect_uris"], []string{"https://app.example.com/mcp/oauth/github/callback"})
	wantStrings(t, doc["grant_types"], []string{"authorization_code", "refresh_token"})
	wantStrings(t, doc["response_types"], []string{"code"})
	if got := doc["token_endpoint_auth_method"]; got != "none" {
		t.Fatalf("token_endpoint_auth_method = %#v, want none", got)
	}
	if got := doc["client_uri"]; got != "https://app.example.com" {
		t.Fatalf("client_uri = %#v", got)
	}
	if got, _ := doc["client_name"].(string); got == "" {
		t.Fatalf("client_name = %#v, want a non-empty name", doc["client_name"])
	}
}

func TestClientMetadataDocumentFallsBackToTheRequestOrigin(t *testing.T) {
	p := OAuthRoutesFor("github", oauth.Config{})
	call := mountModule(t, p)

	rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/github/client-metadata.json")

	doc := decodeDocument(t, rec)
	if got := doc["client_id"]; got != "http://localhost:4000/mcp/oauth/github/client-metadata.json" {
		t.Fatalf("client_id = %#v", got)
	}
	wantStrings(t, doc["redirect_uris"], []string{"http://localhost:4000/mcp/oauth/github/callback"})
	// This document was assembled from the Host and X-Forwarded-Proto headers,
	// which a shared cache does not key on, so it must never be stored: a
	// request carrying another Host would otherwise be served back to an
	// authorization server as this application's description.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store for a request-derived document", got)
	}
}

func TestClientMetadataDocumentFollowsAnUnauthenticatedForwardedProto(t *testing.T) {
	// X-Forwarded-Proto is attacker-controlled unless a trusted proxy sets it,
	// so the scheme it produces is another reason the fallback document is not
	// cacheable. Pinning the public URL removes the header from the picture.
	tests := []struct {
		name      string
		options   []Option
		forwarded string
		wantID    string
		wantCache string
	}{
		{
			name:      "forwarded proto is reflected when nothing is pinned",
			forwarded: "https",
			wantID:    "https://app.example.com/mcp/oauth/proto/client-metadata.json",
			wantCache: "no-store",
		},
		{
			name:      "forwarded proto is ignored when the public url is pinned",
			options:   []Option{WithPublicURL("http://app.example.com")},
			forwarded: "https",
			wantID:    "http://app.example.com/mcp/oauth/proto/client-metadata.json",
			wantCache: "public, max-age=3600",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := OAuthRoutesFor("proto", oauth.Config{}, tt.options...)
			r := router.NewV2()
			p.Routes(chain.NewRouting(r, chain.NewMiddlewareStack(&velapp.Services{})))

			req := httptest.NewRequest(http.MethodGet, "http://app.example.com/mcp/oauth/proto/client-metadata.json", nil)
			req.Header.Set("X-Forwarded-Proto", tt.forwarded)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if got := decodeDocument(t, rec)["client_id"]; got != tt.wantID {
				t.Fatalf("client_id = %#v, want %q", got, tt.wantID)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Fatalf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
		})
	}
}

func TestClientMetadataDocumentCustomisation(t *testing.T) {
	p := OAuthRoutesFor("github", oauth.Config{},
		WithPublicURL("https://app.example.com"),
		WithClientMetadata(map[string]any{
			// Descriptive fields are published as given.
			"client_name": "Acme Dashboard",
			"logo_uri":    "https://app.example.com/logo.png",
			"client_uri":  "https://acme.example.com",
			"contacts":    []string{"ops@acme.example.com"},
			// Extra redirect URIs join the generated callback.
			"redirect_uris": []string{"https://app.example.com/callback", "https://app.example.com/mcp/oauth/github/callback"},
			// Computed fields cannot be weakened.
			"client_id":                  "https://evil.example.net/impostor.json",
			"token_endpoint_auth_method": "client_secret_post",
			// Credentials are never published.
			"client_secret":             "nope",
			"client_secret_expires_at":  0,
			"registration_access_token": "nope",
		}))
	call := mountModule(t, p)

	doc := decodeDocument(t, call(http.MethodGet, "https://app.example.com/mcp/oauth/github/client-metadata.json"))

	if got := doc["client_name"]; got != "Acme Dashboard" {
		t.Fatalf("client_name = %#v", got)
	}
	if got := doc["logo_uri"]; got != "https://app.example.com/logo.png" {
		t.Fatalf("logo_uri = %#v", got)
	}
	if got := doc["client_uri"]; got != "https://acme.example.com" {
		t.Fatalf("client_uri = %#v", got)
	}
	wantStrings(t, doc["contacts"], []string{"ops@acme.example.com"})
	if got := doc["client_id"]; got != "https://app.example.com/mcp/oauth/github/client-metadata.json" {
		t.Fatalf("client_id was overridden: %#v", got)
	}
	if got := doc["token_endpoint_auth_method"]; got != "none" {
		t.Fatalf("token_endpoint_auth_method was overridden: %#v", got)
	}
	// The generated callback comes first, the declared extra follows, and the
	// duplicate of the callback is dropped.
	wantStrings(t, doc["redirect_uris"], []string{
		"https://app.example.com/mcp/oauth/github/callback",
		"https://app.example.com/callback",
	})
	for _, key := range []string{"client_secret", "client_secret_expires_at", "registration_access_token"} {
		if _, present := doc[key]; present {
			t.Fatalf("document published a credential field %q: %v", key, doc)
		}
	}
}

func TestClientMetadataDocumentPathOverride(t *testing.T) {
	p := OAuthRoutesFor("github", oauth.Config{},
		WithPublicURL("https://app.example.com"),
		WithClientMetadataPath("oauth/github/metadata.json"))
	call := mountModule(t, p)

	if rec := call(http.MethodGet, "https://app.example.com/mcp/oauth/github/client-metadata.json"); rec.Code == http.StatusOK {
		t.Fatalf("default path should not be mounted, got %d", rec.Code)
	}
	rec := call(http.MethodGet, "https://app.example.com/oauth/github/metadata.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	doc := decodeDocument(t, rec)
	if got := doc["client_id"]; got != "https://app.example.com/oauth/github/metadata.json" {
		t.Fatalf("client_id = %#v, want the overridden path", got)
	}
}

func TestClientMetadataRouteSkipsTheWebStack(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("mwflow", as.URL+"/mcp")
	p := OAuthRoutesFor("mwflow", oauth.Config{ClientID: "cid", Issuer: as.URL}, WithStore(NewMemoryStore()))

	marker := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			c.SetHeader("X-Web-Middleware", "ran")
			return next(c)
		}
	}
	call := mountModule(t, p, marker)

	// The browser-facing routes run behind the web stack.
	redirect := call(http.MethodGet, "http://localhost:4000/mcp/oauth/mwflow/redirect")
	if redirect.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302; body: %s", redirect.Code, redirect.Body.String())
	}
	if got := redirect.Header().Get("X-Web-Middleware"); got != "ran" {
		t.Fatalf("web middleware did not run on the redirect route (header %q)", got)
	}
	callback := call(http.MethodGet, "http://localhost:4000/mcp/oauth/mwflow/callback?state=none")
	if got := callback.Header().Get("X-Web-Middleware"); got != "ran" {
		t.Fatalf("web middleware did not run on the callback route (header %q)", got)
	}

	// The authorization server fetches the document without a session, so no
	// web middleware may stand in front of it.
	document := call(http.MethodGet, "http://localhost:4000/mcp/oauth/mwflow/client-metadata.json")
	if document.Code != http.StatusOK {
		t.Fatalf("document status = %d, want 200", document.Code)
	}
	if got := document.Header().Get("X-Web-Middleware"); got != "" {
		t.Fatalf("web middleware ran on the client metadata route (header %q)", got)
	}
}
