package mcpclient

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/auth"
	"github.com/velocitykode/velocity/auth/drivers/schemes"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/crypto"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// A pending authorization carries the PKCE verifier and the client secret, and
// the default store keeps it in the velocity session. velocity's own session
// store is a cookie, so the record does travel to the browser: what keeps it
// from being exposed there is that the cookie is sealed with the application's
// key. This drives the redirect leg over that store, the real one and not a
// double, and reads the cookie the browser is handed the way the browser would.
func TestSessionStoreNeverHandsTheBrowserAReadablePendingAuthorization(t *testing.T) {
	const secret = "configured-client-secret-0123456789"

	encryptor, err := crypto.NewEncryptor(crypto.Config{Key: strings.Repeat("k", 32), Cipher: "AES-256-GCM"})
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	scheme, err := schemes.NewSessionScheme(nil, auth.SessionConfig{
		Name: "app_session", Lifetime: 120, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}, encryptor)
	if err != nil {
		t.Fatalf("session scheme: %v", err)
	}
	manager := auth.NewManager()
	manager.RegisterScheme("web", scheme)
	manager.SetDefaultScheme("web")
	services := &velapp.Services{Auth: manager}

	as := fakeAS(t)
	RegisterClient("sealed", as.URL+"/mcp")
	p := OAuthRoutesFor("sealed", oauth.Config{ClientID: "cid", ClientSecret: secret, Issuer: as.URL})
	r := router.NewV2()
	r.SetServices(services)
	stack := chain.NewMiddlewareStack(services)
	stack.Web(scheme.SessionMiddleware())
	p.Routes(chain.NewRouting(r, stack))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:4000/mcp/oauth/sealed/redirect", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("redirect status = %d; body: %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state := location.Query().Get("state")

	var session *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "app_session" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatalf("no session cookie was set: %v", rec.Header()["Set-Cookie"])
	}

	// The record is in the cookie: opened with the application's key it holds
	// the secret and the state of this flow. Without that the rest of the test
	// would hold for a store that simply saved nothing.
	opened, err := encryptor.Decrypt(session.Value)
	if err != nil {
		t.Fatalf("the session cookie does not open with the application key: %v", err)
	}
	for _, carried := range []string{secret, state} {
		if !strings.Contains(opened, carried) {
			t.Fatalf("the session does not carry %q, so this test proves nothing about it", carried)
		}
	}

	// What the browser holds is the sealed value. Neither the secret nor the
	// state can be read out of it, as they stand or under the encodings a cookie
	// value is usually wrapped in.
	readable := []string{session.Value}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(session.Value); err == nil {
			readable = append(readable, string(decoded))
		}
	}
	if unescaped, err := url.QueryUnescape(session.Value); err == nil {
		readable = append(readable, unescaped)
	}
	for _, text := range readable {
		for _, hidden := range []string{secret, state, "Verifier", "ClientSecret"} {
			if strings.Contains(text, hidden) {
				t.Fatalf("the session cookie exposes %q to the browser", hidden)
			}
		}
	}
	if !session.HttpOnly {
		t.Fatal("the session cookie is readable by scripts")
	}
}

// storeClock is a clock a test moves by hand.
type storeClock struct{ at time.Time }

func (k *storeClock) now() time.Time { return k.at }

// savePendingAs saves a pending authorization for the browser holding sid (or a
// new browser when sid is nil) and returns that browser's cookie.
func savePendingAs(t *testing.T, store *MemoryStore, sid *http.Cookie, state string) *http.Cookie {
	t.Helper()
	var c *router.Context
	var rec *httptest.ResponseRecorder
	if sid == nil {
		c, rec = ctxWithCookies()
	} else {
		c, rec = ctxWithCookies(sid)
	}
	if err := store.SavePending(c, &oauth.PendingAuthorization{State: state, Verifier: "verifier-" + state, ClientSecret: "secret"}); err != nil {
		t.Fatalf("save %q: %v", state, err)
	}
	if sid == nil {
		sid = setCookie(rec)
	}
	return sid
}

// A pending authorization holds a PKCE verifier and a client secret for a flow
// that may never come back: the user closes the tab, or nobody was there to
// begin with, since starting a flow takes one request. The memory store has to
// let go of such a record on its own, and cannot wait for a callback to do it.
func TestMemoryStoreForgetsAbandonedAuthorizations(t *testing.T) {
	tests := []struct {
		name      string
		after     time.Duration
		wantTaken bool
	}{
		{name: "taken at once", after: 0, wantTaken: true},
		{name: "taken just inside its lifetime", after: 15*time.Minute - time.Nanosecond, wantTaken: true},
		{name: "refused at the end of its lifetime", after: 15 * time.Minute},
		{name: "refused long after", after: 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &storeClock{at: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
			store := NewMemoryStore()
			store.now = clock.now

			sid := savePendingAs(t, store, nil, "the-state")
			clock.at = clock.at.Add(tt.after)

			c, _ := ctxWithCookies(sid)
			got, err := store.TakePending(c, "the-state")
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			if (got != nil) != tt.wantTaken {
				t.Fatalf("taken = %v, want %v", got != nil, tt.wantTaken)
			}
			if tt.wantTaken && got.Verifier != "verifier-the-state" {
				t.Fatalf("pending = %+v", got)
			}
			// Taken or lapsed, the record is gone either way.
			if held := len(store.pending); held != 0 {
				t.Fatalf("the store still holds %d record(s)", held)
			}
		})
	}
}

func TestMemoryStoreDropsLapsedAuthorizationsWithoutACallback(t *testing.T) {
	clock := &storeClock{at: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	store := NewMemoryStore()
	store.now = clock.now

	for i := 0; i < 50; i++ {
		savePendingAs(t, store, nil, fmt.Sprintf("abandoned-%d", i))
	}
	if held := len(store.pending); held != 50 {
		t.Fatalf("held = %d, want the 50 flows just started", held)
	}

	// No callback ever arrives. The next flow to start is what clears them.
	clock.at = clock.at.Add(15 * time.Minute)
	sid := savePendingAs(t, store, nil, "live")
	if held := len(store.pending); held != 1 {
		t.Fatalf("held = %d after the lifetime passed, want only the flow just started", held)
	}
	c, _ := ctxWithCookies(sid)
	if got, _ := store.TakePending(c, "live"); got == nil {
		t.Fatal("the live flow was dropped along with the lapsed ones")
	}
}

func TestMemoryStoreBoundsTheAuthorizationsItHolds(t *testing.T) {
	clock := &storeClock{at: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	store := NewMemoryStore()
	store.now = clock.now

	// One browser starts flow after flow, a second apart, all inside the
	// lifetime, so nothing lapses and only the bound can keep the store small.
	first := savePendingAs(t, store, nil, "flow-0")
	const started = 1024 + 25
	for i := 1; i < started; i++ {
		clock.at = clock.at.Add(time.Millisecond)
		savePendingAs(t, store, first, fmt.Sprintf("flow-%d", i))
	}

	if held := len(store.pending); held != 1024 {
		t.Fatalf("held = %d, want the bound of 1024", held)
	}
	for _, tt := range []struct {
		state     string
		wantTaken bool
	}{
		{state: "flow-0"},
		{state: "flow-24"},
		{state: "flow-25", wantTaken: true},
		{state: fmt.Sprintf("flow-%d", started-1), wantTaken: true},
	} {
		c, _ := ctxWithCookies(first)
		got, err := store.TakePending(c, tt.state)
		if err != nil {
			t.Fatalf("take %q: %v", tt.state, err)
		}
		if (got != nil) != tt.wantTaken {
			t.Fatalf("%q taken = %v, want %v: the oldest records make room, the newest stay", tt.state, got != nil, tt.wantTaken)
		}
	}
}

// The memory store's cookie is the whole of a browser's claim to its token, so
// it carries the Secure attribute on the same terms as the framework's own
// cookies: always, unless the application's validated session-cookie
// configuration opted out of it, which velocity allows outside production only.
func TestMemoryStoreCookieIsSecureUnlessTheApplicationOptedOut(t *testing.T) {
	tests := []struct {
		name       string
		services   *velapp.Services
		wantSecure bool
	}{
		{name: "no services wired", services: nil, wantSecure: true},
		{name: "services with the default cookie posture", services: &velapp.Services{}, wantSecure: true},
		{name: "services that opted out for development", services: &velapp.Services{InsecureFlashCookies: true}, wantSecure: false},
	}
	writes := []struct {
		name  string
		write func(store *MemoryStore, c *router.Context) error
	}{
		{name: "minted while saving a pending authorization", write: func(store *MemoryStore, c *router.Context) error {
			return store.SavePending(c, &oauth.PendingAuthorization{State: "s"})
		}},
		{name: "minted while saving a token", write: func(store *MemoryStore, c *router.Context) error {
			return store.SaveToken(c, "acme", "tok")
		}},
	}

	for _, tt := range tests {
		for _, w := range writes {
			t.Run(tt.name+"/"+w.name, func(t *testing.T) {
				c, rec := ctxWithCookies()
				if tt.services != nil {
					c.SetServices(tt.services)
				}
				if err := w.write(NewMemoryStore(), c); err != nil {
					t.Fatalf("write: %v", err)
				}
				cookie := setCookie(rec)
				if cookie == nil || cookie.Value == "" {
					t.Fatal("no browser cookie was set")
				}
				if cookie.Secure != tt.wantSecure {
					t.Fatalf("Secure = %v, want %v", cookie.Secure, tt.wantSecure)
				}
				if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
					t.Fatalf("cookie = %+v, want HttpOnly, SameSite=Lax, Path=/", cookie)
				}
			})
		}
	}
}
