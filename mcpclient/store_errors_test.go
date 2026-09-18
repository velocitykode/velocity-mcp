package mcpclient

import (
	"errors"
	"net/http"
	"testing"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

func TestSessionStoreWithoutASessionRefusesEveryOperation(t *testing.T) {
	c, _ := ctxWithCookies() // no auth manager, so no session
	store := SessionStore{}

	if err := store.SavePending(c, &oauth.PendingAuthorization{State: "s"}); err == nil {
		t.Fatal("SavePending must refuse without a session")
	}
	if _, err := store.TakePending(c, "s"); err == nil {
		t.Fatal("TakePending must refuse without a session")
	}
	if err := store.SaveToken(c, "acme", "t"); err == nil {
		t.Fatal("SaveToken must refuse without a session")
	}
	if _, err := store.Token(c, "acme"); err == nil {
		t.Fatal("Token must refuse without a session")
	}
}

// failingStore reports a failure from the store operation under test.
type failingStore struct {
	onSavePending error
	onTakePending error
	onSaveToken   error
	pending       *oauth.PendingAuthorization
}

func (f *failingStore) SavePending(c *router.Context, p *oauth.PendingAuthorization) error {
	f.pending = p
	return f.onSavePending
}

func (f *failingStore) TakePending(c *router.Context, state string) (*oauth.PendingAuthorization, error) {
	if f.onTakePending != nil {
		return nil, f.onTakePending
	}
	return f.pending, nil
}

func (f *failingStore) SaveToken(c *router.Context, name, token string) error { return f.onSaveToken }

func (f *failingStore) Token(c *router.Context, name string) (string, error) { return "", nil }

func TestRedirectReportsAStoreFailure(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("storefail", as.URL+"/mcp")
	p := OAuthRoutesFor("storefail", oauth.Config{ClientID: "cid", Issuer: as.URL},
		WithStore(&failingStore{onSavePending: errors.New("session write failed")}))
	call := mountModule(t, p)

	rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/storefail/redirect")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("the browser must not be redirected when the flow state was not persisted")
	}
}

func TestCallbackReportsStoreAndExchangeFailures(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("cbfail", as.URL+"/mcp")

	t.Run("pending lookup fails", func(t *testing.T) {
		p := OAuthRoutesFor("cbfail", oauth.Config{ClientID: "cid", Issuer: as.URL},
			WithStore(&failingStore{onTakePending: errors.New("session read failed")}))
		call := mountModule(t, p)
		rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/cbfail/callback?code=x&state=s")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("code exchange fails", func(t *testing.T) {
		store := &failingStore{}
		p := OAuthRoutesFor("cbfail", oauth.Config{ClientID: "cid", Issuer: as.URL}, WithStore(store))
		call := mountModule(t, p)

		if rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/cbfail/redirect"); rec.Code != http.StatusFound {
			t.Fatalf("redirect status = %d", rec.Code)
		}
		// The callback carries a state that does not match the pending flow.
		rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/cbfail/callback?code=x&state=wrong")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("token storage fails", func(t *testing.T) {
		store := &failingStore{onSaveToken: errors.New("session write failed")}
		p := OAuthRoutesFor("cbfail", oauth.Config{ClientID: "cid", Issuer: as.URL}, WithStore(store))
		call := mountModule(t, p)

		redirect := call(http.MethodGet, "http://localhost:4000/mcp/oauth/cbfail/redirect")
		if redirect.Code != http.StatusFound {
			t.Fatalf("redirect status = %d", redirect.Code)
		}
		state := store.pending.State
		rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/cbfail/callback?code=x&state="+state+"&iss="+as.URL)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
		}
	})
}
