package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	c := NewClient(Config{ClientID: "cid", Issuer: a.srv.URL, RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")

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
	c := NewClient(Config{ClientID: "cid", Issuer: a.srv.URL}, a.srv.URL, "", "")
	if _, _, err := c.AuthorizationURL(context.Background(), ""); err == nil {
		t.Fatal("expected error without redirect uri")
	}
}

func TestExchangeCodeSuccess(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", ClientSecret: "secret", Issuer: a.srv.URL, RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")

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
	c := NewClient(Config{ClientID: "cid", Issuer: a.srv.URL, RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")
	_, pending, _ := c.AuthorizationURL(context.Background(), "")

	cb := url.Values{"code": {"x"}, "state": {"WRONG"}}
	if _, _, err := c.ExchangeCode(context.Background(), pending, cb); err == nil {
		t.Fatal("expected state mismatch error")
	}
}

func TestExchangeCodeServerError(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", Issuer: a.srv.URL, RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")
	_, pending, _ := c.AuthorizationURL(context.Background(), "")

	// The error is reported because the response carries the state of this
	// flow; one that does not is covered by
	// TestExchangeCodeValidatesTheResponseBeforeReadingIt.
	cb := url.Values{"error": {"access_denied"}, "error_description": {"user said no"}, "state": {pending.State}}
	_, _, err := c.ExchangeCode(context.Background(), pending, cb)
	want := "the authorization server returned an error [access_denied]: user said no"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestExchangeCodeIssuerMismatch(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", Issuer: a.srv.URL, RedirectURI: "http://localhost/callback"}, a.srv.URL, "", "")
	_, pending, _ := c.AuthorizationURL(context.Background(), "")

	cb := url.Values{"code": {"x"}, "state": {pending.State}, "iss": {"https://evil.example.com"}}
	if _, _, err := c.ExchangeCode(context.Background(), pending, cb); err == nil {
		t.Fatal("expected issuer mismatch error")
	}
}

func TestClientCredentials(t *testing.T) {
	a := newAuthServer(t)
	c := NewClient(Config{ClientID: "cid", ClientSecret: "secret", Issuer: a.srv.URL}, a.srv.URL, "", "")

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
	c := NewClient(Config{ClientID: "cid", ClientSecret: "secret", Issuer: a.srv.URL}, a.srv.URL, "", "")

	token, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "old-refresh", Issuer: a.srv.URL})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if token.AccessToken != "access-123" || token.ClientID != "cid" {
		t.Fatalf("token = %+v", token)
	}
	if token.Issuer != a.srv.URL {
		t.Fatalf("refreshed token issuer = %q, want %q", token.Issuer, a.srv.URL)
	}
	if a.lastTokenReq.Get("grant_type") != "refresh_token" || a.lastTokenReq.Get("refresh_token") != "old-refresh" {
		t.Fatalf("token request = %v", a.lastTokenReq)
	}
}

// scopeServer is a protected resource and authorization server in one whose
// scopes_supported member the test writes out as raw JSON, so it can be a list,
// something that is not a list, or left out. It records the token request and
// the registration request it receives.
type scopeServer struct {
	srv          *httptest.Server
	tokenForms   chan url.Values
	registration chan map[string]any
}

func newScopeServer(t *testing.T, scopesSupported string) *scopeServer {
	t.Helper()
	s := &scopeServer{tokenForms: make(chan url.Values, 4), registration: make(chan map[string]any, 4)}
	mux := http.NewServeMux()
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		member := ""
		if scopesSupported != "" {
			member = `,"scopes_supported":` + scopesSupported
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource":"` + s.srv.URL + `","authorization_servers":["` + s.srv.URL + `"]` + member + `}`))
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           s.srv.URL,
			"authorization_endpoint":           s.srv.URL + "/authorize",
			"token_endpoint":                   s.srv.URL + "/token",
			"registration_endpoint":            s.srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.registration <- body
		writeJSON(w, map[string]any{"client_id": "dyn-client"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.tokenForms <- r.Form
		writeJSON(w, map[string]any{"access_token": "access-123", "token_type": "Bearer", "refresh_token": "refresh-123"})
	})
	return s
}

// The MCP authorization specification's scope selection strategy: the scope the
// server named in its challenge, otherwise every scope the protected resource
// lists in scopes_supported, and no scope parameter at all when it lists none.
// The consumer's own Config.Scope sits between the two. What the strategy rules
// out is inventing a scope: an authorization server that has never heard of it
// answers invalid_scope, and the flow fails for a server that needed none.
//
// Every request that carries a scope is checked, because the same selection
// feeds all of them: the authorization request, the client credentials grant,
// and the scope a dynamic registration asks to be allowed.
func TestScopeSelection(t *testing.T) {
	tests := []struct {
		name            string
		challenge       string
		configured      string
		scopesSupported string
		// want is the scope sent; "" means the parameter is absent altogether.
		want string
	}{
		{
			name:            "the challenge scope wins over everything",
			challenge:       "files:read",
			configured:      "configured",
			scopesSupported: `["files:read","files:write"]`,
			want:            "files:read",
		},
		{
			name:            "the configured scope stands when the challenge names none",
			configured:      "configured",
			scopesSupported: `["files:read","files:write"]`,
			want:            "configured",
		},
		{
			name:            "every supported scope when neither names one",
			scopesSupported: `["files:read","files:write"]`,
			want:            "files:read files:write",
		},
		{
			name:            "a single supported scope",
			scopesSupported: `["mcp:use"]`,
			want:            "mcp:use",
		},
		{name: "no scope parameter when the resource lists none", scopesSupported: ``},
		{name: "no scope parameter for an empty list", scopesSupported: `[]`},
		{name: "no scope parameter for a null list", scopesSupported: `null`},
		{name: "no scope parameter when the member is not a list", scopesSupported: `"files:read files:write"`},
		{name: "no scope parameter when the member is an object", scopesSupported: `{"files:read":true}`},
		{
			name:            "entries that are not scope tokens are left out",
			scopesSupported: `["files:read","two words","quo\"te","back\\slash","new\nline","nul\u0000","caf\u00e9","",42,null,["nested"],"files:write"]`,
			want:            "files:read files:write",
		},
		{
			name:            "a repeated scope is requested once",
			scopesSupported: `["files:read","files:write","files:read"]`,
			want:            "files:read files:write",
		},
		{
			name:            "a list of nothing requestable",
			scopesSupported: `["two words",""]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("authorization request and registration", func(t *testing.T) {
				server := newScopeServer(t, tt.scopesSupported)
				c := NewClient(Config{Scope: tt.configured, RedirectURI: "https://app.example.com/callback"}, server.srv.URL, "", tt.challenge)

				query := authorizeQuery(t, c)
				if got := query.Get("scope"); got != tt.want || query.Has("scope") != (tt.want != "") {
					t.Fatalf("authorization request scope = %q (present: %v), want %q", got, query.Has("scope"), tt.want)
				}

				registered := <-server.registration
				got, present := registered["scope"]
				if present != (tt.want != "") || (present && got != tt.want) {
					t.Fatalf("registration scope = %#v (present: %v), want %q", got, present, tt.want)
				}
			})

			t.Run("client credentials grant", func(t *testing.T) {
				server := newScopeServer(t, tt.scopesSupported)
				c := NewClient(Config{ClientID: "cid", Issuer: server.srv.URL, Scope: tt.configured}, server.srv.URL, "", tt.challenge)

				if _, err := c.ClientCredentials(context.Background()); err != nil {
					t.Fatalf("client credentials: %v", err)
				}
				form := <-server.tokenForms
				if got := form.Get("scope"); got != tt.want || form.Has("scope") != (tt.want != "") {
					t.Fatalf("token request scope = %q (present: %v), want %q", got, form.Has("scope"), tt.want)
				}
			})
		})
	}
}

// A refresh may not ask for a scope beyond what was granted, and leaving the
// parameter out means exactly what was granted (RFC 6749 6). The scope a fresh
// authorization would select is no record of what this grant got, so a refresh
// sends none, whatever the challenge, the configuration or the resource names.
func TestRefreshSendsNoScope(t *testing.T) {
	server := newScopeServer(t, `["files:read","files:write"]`)
	c := NewClient(Config{ClientID: "cid", Issuer: server.srv.URL, Scope: "configured"}, server.srv.URL, "", "challenged")

	if _, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "refresh-123", Issuer: server.srv.URL, Scope: "files:read"}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	form := <-server.tokenForms
	if form.Has("scope") {
		t.Fatalf("refresh sent scope %q, want none", form.Get("scope"))
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
}
