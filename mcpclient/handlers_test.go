package mcpclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// fakeAS is a minimal authorization server + token endpoint for handler tests.
func fakeAS(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := func() string { return srv.URL }

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              base() + "/mcp",
			"authorization_servers": []string{base()},
			"scopes_supported":      []string{"mcp:use"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                         base(),
			"authorization_endpoint":                         base() + "/authorize",
			"token_endpoint":                                 base() + "/token",
			"code_challenge_methods_supported":               []string{"S256"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"access_token": "issued-token", "token_type": "Bearer", "expires_in": 3600})
	})
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestHandlersFullFlow(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("flow", as.URL+"/mcp")
	mem := NewMemoryStore()
	p := OAuthRoutesFor("flow", oauth.Config{ClientID: "cid", Scope: "mcp:use"},
		WithStore(mem), WithSuccessRedirect("/ok"))

	// --- redirect leg ---
	reqR := httptest.NewRequest(http.MethodGet, "http://localhost:4000/mcp/oauth/flow/redirect", nil)
	recR := httptest.NewRecorder()
	if err := p.handleRedirect(router.NewContext(recR, reqR)); err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if recR.Code != http.StatusFound {
		t.Fatalf("redirect status = %d", recR.Code)
	}
	loc := recR.Header().Get("Location")
	if !strings.HasPrefix(loc, as.URL+"/authorize") {
		t.Fatalf("location = %q", loc)
	}
	locURL, _ := url.Parse(loc)
	state := locURL.Query().Get("state")
	if state == "" || locURL.Query().Get("code_challenge") == "" {
		t.Fatalf("missing state/pkce in %q", loc)
	}
	sid := setCookie(recR)
	if sid == nil {
		t.Fatal("redirect should set a browser cookie for the memory store")
	}

	// --- callback leg (same browser cookie) ---
	cbURL := "http://localhost:4000/mcp/oauth/flow/callback?code=abc&state=" + state + "&iss=" + url.QueryEscape(as.URL)
	reqC := httptest.NewRequest(http.MethodGet, cbURL, nil)
	reqC.AddCookie(sid)
	recC := httptest.NewRecorder()
	if err := p.handleCallback(router.NewContext(recC, reqC)); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if recC.Code != http.StatusFound || recC.Header().Get("Location") != "/ok" {
		t.Fatalf("callback status=%d loc=%q", recC.Code, recC.Header().Get("Location"))
	}

	// Token persisted for that browser.
	cTok, _ := ctxWithCookies(sid)
	if tok, _ := mem.Token(cTok, "flow"); tok != "issued-token" {
		t.Fatalf("stored token = %q", tok)
	}
}

func TestHandlerCallbackUnknownState(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("flow2", as.URL+"/mcp")
	p := OAuthRoutesFor("flow2", oauth.Config{ClientID: "cid"}, WithStore(NewMemoryStore()))

	req := httptest.NewRequest(http.MethodGet, "http://localhost:4000/mcp/oauth/flow2/callback?code=x&state=bogus", nil)
	rec := httptest.NewRecorder()
	if err := p.handleCallback(router.NewContext(rec, req)); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown state, got %d", rec.Code)
	}
}

func TestTokenAndAuthorizedHelpers(t *testing.T) {
	prev := defaultStore
	mem := NewMemoryStore()
	SetDefaultStore(mem)
	defer SetDefaultStore(prev)

	cSave, recSave := ctxWithCookies()
	_ = mem.SaveToken(cSave, "h", "tok-h")
	sid := setCookie(recSave)

	c, _ := ctxWithCookies(sid)
	if tok, ok := Token(c, "h"); !ok || tok != "tok-h" {
		t.Fatalf("Token = %q ok=%v", tok, ok)
	}
	if !Authorized(c, "h") {
		t.Fatal("Authorized should be true")
	}
	cNone, _ := ctxWithCookies()
	if Authorized(cNone, "h") {
		t.Fatal("Authorized should be false without token")
	}
}
