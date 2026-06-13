package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// authServer is a fake authorization server: it serves protected-resource and
// authorization-server metadata plus token and registration endpoints.
type authServer struct {
	srv          *httptest.Server
	lastTokenReq url.Values
	registered   bool
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	a := &authServer{}
	mux := http.NewServeMux()
	a.srv = httptest.NewServer(mux)

	base := func() string { return a.srv.URL }

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              base(),
			"authorization_servers": []string{base()},
			"scopes_supported":      []string{"mcp:use"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                base(),
			"authorization_endpoint":                base() + "/authorize",
			"token_endpoint":                        base() + "/token",
			"registration_endpoint":                 base() + "/register",
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		a.registered = true
		writeJSON(w, map[string]any{"client_id": "dyn-client", "client_secret": "dyn-secret"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		a.lastTokenReq = r.Form
		writeJSON(w, map[string]any{
			"access_token":  "access-123",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "refresh-123",
			"scope":         "mcp:use",
		})
	})
	t.Cleanup(a.srv.Close)
	return a
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestAuthorizationURLWithConfiguredClient(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")

	authURL, pending, err := c.AuthorizationURL(context.Background(), "/dashboard")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
		t.Fatalf("authorize query = %v", q)
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("missing pkce/state: %v", q)
	}
	if pending.State != q.Get("state") || pending.Verifier == "" || pending.ReturnTo != "/dashboard" {
		t.Fatalf("pending = %+v", pending)
	}
	if a.registered {
		t.Fatal("should not register dynamically when client_id is configured")
	}
}

func TestAuthorizationURLDynamicRegistration(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")

	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	if !a.registered || pending.ClientID != "dyn-client" {
		t.Fatalf("expected dynamic registration, pending=%+v registered=%v", pending, a.registered)
	}
}

func TestAuthorizationURLRequiresRedirect(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid"}, a.srv.URL, "", "")
	if _, _, err := c.AuthorizationURL(context.Background(), ""); err == nil {
		t.Fatal("expected error without redirect uri")
	}
}

func TestExchangeCodeSuccess(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", ClientSecret: "secret", RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")

	_, pending, err := c.AuthorizationURL(context.Background(), "/back")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}

	cb := url.Values{"code": {"the-code"}, "state": {pending.State}, "iss": {a.srv.URL}}
	token, returnTo, err := c.ExchangeCode(context.Background(), pending, cb)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if token.AccessToken != "access-123" || token.RefreshToken != "refresh-123" {
		t.Fatalf("token = %+v", token)
	}
	if returnTo != "/back" {
		t.Fatalf("returnTo = %q", returnTo)
	}
	if token.ExpiresAt.IsZero() {
		t.Fatal("expected expiry to be set from expires_in")
	}
	// The code and verifier were posted to the token endpoint.
	if a.lastTokenReq.Get("code") != "the-code" || a.lastTokenReq.Get("code_verifier") != pending.Verifier {
		t.Fatalf("token request = %v", a.lastTokenReq)
	}
}

func TestExchangeCodeStateMismatch(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")
	_, pending, _ := c.AuthorizationURL(context.Background(), "")

	cb := url.Values{"code": {"x"}, "state": {"WRONG"}}
	if _, _, err := c.ExchangeCode(context.Background(), pending, cb); err == nil {
		t.Fatal("expected state mismatch error")
	}
}

func TestExchangeCodeServerError(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")
	_, pending, _ := c.AuthorizationURL(context.Background(), "")

	cb := url.Values{"error": {"access_denied"}, "error_description": {"user said no"}}
	_, _, err := c.ExchangeCode(context.Background(), pending, cb)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("expected access_denied error, got %v", err)
	}
}

func TestExchangeCodeIssuerMismatch(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")
	_, pending, _ := c.AuthorizationURL(context.Background(), "")

	cb := url.Values{"code": {"x"}, "state": {pending.State}, "iss": {"https://evil.example.com"}}
	if _, _, err := c.ExchangeCode(context.Background(), pending, cb); err == nil {
		t.Fatal("expected issuer mismatch error")
	}
}

func TestClientCredentials(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", ClientSecret: "secret"}, a.srv.URL, "", "")

	token, err := c.ClientCredentials(context.Background())
	if err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	if token.AccessToken != "access-123" {
		t.Fatalf("token = %+v", token)
	}
	if a.lastTokenReq.Get("grant_type") != "client_credentials" {
		t.Fatalf("grant_type = %q", a.lastTokenReq.Get("grant_type"))
	}
}

func TestClientCredentialsRequiresClientID(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{}, a.srv.URL, "", "")
	if _, err := c.ClientCredentials(context.Background()); err == nil {
		t.Fatal("expected error without client_id")
	}
}

func TestRefresh(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", ClientSecret: "secret"}, a.srv.URL, "", "")

	token, err := c.Refresh(context.Background(), "old-refresh", "", "")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if token.AccessToken != "access-123" || token.ClientID != "cid" {
		t.Fatalf("token = %+v", token)
	}
	if a.lastTokenReq.Get("grant_type") != "refresh_token" || a.lastTokenReq.Get("refresh_token") != "old-refresh" {
		t.Fatalf("token request = %v", a.lastTokenReq)
	}
}

func TestScopePrecedence(t *testing.T) {
	// Challenge scope wins over config scope.
	c := NewClient(Config{Scope: "config-scope"}, "https://example.com", "", "challenge-scope")
	if got := c.resolveScope(); got != "challenge-scope" {
		t.Fatalf("scope = %q", got)
	}
	c = NewClient(Config{Scope: "config-scope"}, "https://example.com", "", "")
	if got := c.resolveScope(); got != "config-scope" {
		t.Fatalf("scope = %q", got)
	}
	c = NewClient(Config{}, "https://example.com", "", "")
	if got := c.resolveScope(); got != DefaultScope {
		t.Fatalf("scope = %q", got)
	}
}
