package mcpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// ctxWithCookies builds a router.Context whose request carries the given cookies.
func ctxWithCookies(cookies ...*http.Cookie) (*router.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "http://localhost:4000/x", nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	return router.NewContext(rec, req), rec
}

// setCookie extracts the first Set-Cookie written to the recorder.
func setCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	resp := rec.Result()
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return nil
	}
	return cookies[0]
}

func TestRegistryRegisterLookup(t *testing.T) {
	RegisterClient("acme", "https://acme.example.com/mcp")
	e, ok := lookup("acme")
	if !ok || e.resourceURL != "https://acme.example.com/mcp" {
		t.Fatalf("lookup = %+v ok=%v", e, ok)
	}
	if _, ok := lookup("missing"); ok {
		t.Fatal("unexpected lookup hit")
	}
}

func TestMemoryStorePendingBoundToBrowser(t *testing.T) {
	store := NewMemoryStore()
	p := &oauth.PendingAuthorization{State: "state-1", Verifier: "v"}

	// Browser A saves a pending; the store mints a sid cookie.
	cA, recA := ctxWithCookies()
	if err := store.SavePending(cA, p); err != nil {
		t.Fatalf("save: %v", err)
	}
	sid := setCookie(recA)
	if sid == nil || sid.Value == "" {
		t.Fatal("expected a session cookie to be set")
	}

	// Browser B (no/other cookie) must NOT be able to take A's pending, even
	// knowing the state value.
	cB, _ := ctxWithCookies()
	if got, _ := store.TakePending(cB, "state-1"); got != nil {
		t.Fatal("pending must be bound to the originating browser")
	}

	// Browser A completes it.
	cA2, _ := ctxWithCookies(sid)
	got, err := store.TakePending(cA2, "state-1")
	if err != nil || got == nil || got.Verifier != "v" {
		t.Fatalf("take = %+v err=%v", got, err)
	}
	// Single-use: a second take finds nothing.
	cA3, _ := ctxWithCookies(sid)
	if again, _ := store.TakePending(cA3, "state-1"); again != nil {
		t.Fatal("pending should be consumed")
	}
}

func TestMemoryStoreToken(t *testing.T) {
	store := NewMemoryStore()

	cA, recA := ctxWithCookies()
	if err := store.SaveToken(cA, "acme", "tok-A"); err != nil {
		t.Fatalf("save token: %v", err)
	}
	sid := setCookie(recA)

	cA2, _ := ctxWithCookies(sid)
	if tok, _ := store.Token(cA2, "acme"); tok != "tok-A" {
		t.Fatalf("token = %q", tok)
	}
	// Different browser sees no token.
	cB, _ := ctxWithCookies()
	if tok, _ := store.Token(cB, "acme"); tok != "" {
		t.Fatalf("cross-browser token leak: %q", tok)
	}
}

func TestSessionStoreNoSession(t *testing.T) {
	// A plain context has no auth manager / session wired.
	c, _ := ctxWithCookies()
	if err := (SessionStore{}).SaveToken(c, "acme", "t"); err == nil {
		t.Fatal("expected errNoSession")
	}
	if _, err := (SessionStore{}).Token(c, "acme"); err == nil {
		t.Fatal("expected errNoSession")
	}
}

func TestOAuthRoutesForOptions(t *testing.T) {
	mem := NewMemoryStore()
	p := OAuthRoutesFor("acme", oauth.Config{ClientID: "cid"},
		WithStore(mem), WithSuccessRedirect("/done"), WithBasePath("/auth"))

	if p.name != "acme" || p.cfg.ClientID != "cid" {
		t.Fatalf("identity = %+v", p)
	}
	if p.store != mem || p.success != "/done" || p.basePath != "/auth" {
		t.Fatalf("options not applied: store=%v success=%q base=%q", p.store == mem, p.success, p.basePath)
	}

	// Defaults.
	d := OAuthRoutesFor("acme", oauth.Config{})
	if d.success != "/" || d.basePath != DefaultBasePath || d.store == nil {
		t.Fatalf("defaults = %+v", d)
	}
}

func TestForUnregistered(t *testing.T) {
	c, _ := ctxWithCookies()
	if _, err := For(c, "nope-not-registered"); err == nil {
		t.Fatal("expected error for unregistered client")
	}
}

func TestForRegistered(t *testing.T) {
	RegisterClient("forclient", "https://f.example.com/mcp")
	c, _ := ctxWithCookies()
	w, err := For(c, "forclient")
	if err != nil || w == nil {
		t.Fatalf("For = %v err=%v", w, err)
	}
}

func TestForAttachesStoredToken(t *testing.T) {
	prev := defaultStore
	mem := NewMemoryStore()
	SetDefaultStore(mem)
	defer SetDefaultStore(prev)

	RegisterClient("tokclient", "https://t.example.com/mcp")
	cSave, rec := ctxWithCookies()
	_ = mem.SaveToken(cSave, "tokclient", "tok")
	sid := setCookie(rec)

	c, _ := ctxWithCookies(sid)
	w, err := For(c, "tokclient") // exercises the WithToken branch
	if err != nil || w == nil {
		t.Fatalf("For = %v err=%v", w, err)
	}
}

func TestProviderLifecycleNoops(t *testing.T) {
	p := OAuthRoutesFor("x", oauth.Config{})
	if err := p.Register(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Boot(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBaseURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test:9000/path", nil)
	c := router.NewContext(httptest.NewRecorder(), req)
	if got := baseURL(c); got != "http://example.test:9000" {
		t.Fatalf("baseURL = %q", got)
	}

	req2 := httptest.NewRequest(http.MethodGet, "http://proxied.test/path", nil)
	req2.Header.Set("X-Forwarded-Proto", "https")
	c2 := router.NewContext(httptest.NewRecorder(), req2)
	if got := baseURL(c2); got != "https://proxied.test" {
		t.Fatalf("baseURL (forwarded) = %q", got)
	}
}
