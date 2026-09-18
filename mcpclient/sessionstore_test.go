package mcpclient

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/auth"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// sessionScheme is an authentication scheme that only does what SessionStore
// needs: hand out one in-memory session for the request.
type sessionScheme struct{ session auth.Session }

func (s *sessionScheme) Session(*http.Request) auth.Session { return s.session }
func (s *sessionScheme) Check(*http.Request) bool           { return true }
func (s *sessionScheme) User(*http.Request) auth.Authenticatable {
	return nil
}
func (s *sessionScheme) ID(*http.Request) interface{} { return nil }
func (s *sessionScheme) SetUserStore(auth.UserStore)  {}
func (s *sessionScheme) Logout(http.ResponseWriter, *http.Request) error {
	return nil
}
func (s *sessionScheme) Login(http.ResponseWriter, *http.Request, auth.Authenticatable, ...bool) error {
	return nil
}
func (s *sessionScheme) LoginByID(http.ResponseWriter, *http.Request, interface{}, ...bool) error {
	return nil
}
func (s *sessionScheme) Attempt(http.ResponseWriter, *http.Request, map[string]interface{}, ...bool) (bool, error) {
	return false, nil
}

// sessionServices builds a service container whose auth manager hands out the
// given session, the way the web middleware stack does in a real application.
func sessionServices(session auth.Session) *velapp.Services {
	manager := auth.NewManager()
	manager.RegisterScheme("web", &sessionScheme{session: session})
	manager.SetDefaultScheme("web")
	return &velapp.Services{Auth: manager}
}

func TestSessionStoreRoundTrip(t *testing.T) {
	session := auth.NewSession("browser-1")
	c := router.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost:4000/x", nil))
	c.SetServices(sessionServices(session))

	store := SessionStore{}
	pending := &oauth.PendingAuthorization{State: "state-1", Verifier: "verifier-1", ClientID: "cid"}
	if err := store.SavePending(c, pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}

	got, err := store.TakePending(c, "state-1")
	if err != nil {
		t.Fatalf("TakePending: %v", err)
	}
	if got == nil || got.Verifier != "verifier-1" || got.ClientID != "cid" {
		t.Fatalf("TakePending = %+v", got)
	}
	// Single use: the entry is gone after it is taken.
	if again, err := store.TakePending(c, "state-1"); err != nil || again != nil {
		t.Fatalf("TakePending again = %+v err=%v, want nil", again, err)
	}
	// An unknown state is a miss, not an error.
	if unknown, err := store.TakePending(c, "never-issued"); err != nil || unknown != nil {
		t.Fatalf("TakePending(unknown) = %+v err=%v", unknown, err)
	}

	if err := store.SaveToken(c, "acme", "tok-1"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if token, err := store.Token(c, "acme"); err != nil || token != "tok-1" {
		t.Fatalf("Token = %q err=%v", token, err)
	}
	if token, err := store.Token(c, "other"); err != nil || token != "" {
		t.Fatalf("Token(other) = %q err=%v, want empty", token, err)
	}
}

func TestSessionStoreRejectsCorruptedPendingState(t *testing.T) {
	session := auth.NewSession("browser-2")
	session.Put(pendingKeyPrefix+"state-1", "{not json")
	c := router.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost:4000/x", nil))
	c.SetServices(sessionServices(session))

	got, err := SessionStore{}.TakePending(c, "state-1")
	if err == nil {
		t.Fatal("a corrupted session entry must surface as an error")
	}
	if got != nil {
		t.Fatalf("TakePending = %+v, want nil", got)
	}
}

func TestFullFlowOnTheSessionStore(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("sessionflow", as.URL+"/mcp")
	session := auth.NewSession("browser-3")
	services := sessionServices(session)

	p := OAuthRoutesFor("sessionflow", oauth.Config{ClientID: "cid", Issuer: as.URL}, WithSuccessRedirect("/done"))
	r := router.NewV2()
	r.SetServices(services)
	p.Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	call := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	redirect := call("http://localhost:4000/mcp/oauth/sessionflow/redirect?return=/back")
	if redirect.Code != http.StatusFound {
		t.Fatalf("redirect status = %d; body: %s", redirect.Code, redirect.Body.String())
	}
	location, err := url.Parse(redirect.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in %q", location)
	}

	callback := call("http://localhost:4000/mcp/oauth/sessionflow/callback?code=abc&state=" +
		url.QueryEscape(state) + "&iss=" + url.QueryEscape(as.URL))
	if callback.Code != http.StatusFound {
		t.Fatalf("callback status = %d; body: %s", callback.Code, callback.Body.String())
	}
	if got := callback.Header().Get("Location"); got != "/back" {
		t.Fatalf("callback redirected to %q, want the requested return target", got)
	}
	if token, _ := (SessionStore{}).Token(router.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil)), "sessionflow"); token != "" {
		t.Fatal("a context without a session must not surface a token")
	}
	c := router.NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost:4000/x", nil))
	c.SetServices(services)
	if token, err := (SessionStore{}).Token(c, "sessionflow"); err != nil || token != "issued-token" {
		t.Fatalf("stored token = %q err=%v", token, err)
	}
}

// A return target is attacker-supplied. Browsers drop TAB, LF and CR while
// parsing a URL, so "/<TAB>/evil.test" is read as "//evil.test": another
// site. Whatever the callback answers must stay on this one.
func TestReturnTargetWithControlBytesStaysOnSite(t *testing.T) {
	as := fakeAS(t)
	RegisterClient("returnguard", as.URL+"/mcp")
	services := sessionServices(auth.NewSession("browser-return"))

	p := OAuthRoutesFor("returnguard", oauth.Config{ClientID: "cid", Issuer: as.URL}, WithSuccessRedirect("/done"))
	r := router.NewV2()
	r.SetServices(services)
	p.Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	call := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	asBrowserReads := strings.NewReplacer("\t", "", "\n", "", "\r", "")

	for _, target := range []string{
		"/\t/evil.test/x",
		"/\n/evil.test/x",
		"/\r/evil.test/x",
		"\t//evil.test/x",
		"/\t\\evil.test/x",
	} {
		redirect := call("http://localhost:4000/mcp/oauth/returnguard/redirect?return=" + url.QueryEscape(target))
		if redirect.Code != http.StatusFound {
			t.Fatalf("return=%q: redirect status = %d; body: %s", target, redirect.Code, redirect.Body.String())
		}
		location, err := url.Parse(redirect.Header().Get("Location"))
		if err != nil {
			t.Fatalf("return=%q: parse Location: %v", target, err)
		}

		callback := call("http://localhost:4000/mcp/oauth/returnguard/callback?code=abc&state=" +
			url.QueryEscape(location.Query().Get("state")) + "&iss=" + url.QueryEscape(as.URL))
		if callback.Code != http.StatusFound {
			t.Fatalf("return=%q: callback status = %d; body: %s", target, callback.Code, callback.Body.String())
		}

		got := callback.Header().Get("Location")
		seen := asBrowserReads.Replace(strings.TrimLeft(got, " "))
		if !strings.HasPrefix(seen, "/") || strings.HasPrefix(seen, "//") || strings.HasPrefix(seen, "/\\") {
			t.Errorf("return=%q: Location %q is read by a browser as %q, which leaves this site", target, got, seen)
		}
		if got != seen {
			t.Errorf("return=%q: Location %q carries control bytes", target, got)
		}
	}
}
