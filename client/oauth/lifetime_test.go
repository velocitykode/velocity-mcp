package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// lifetimeAS is a fake authorization server whose advertised support for client
// ID metadata documents, the registration endpoint it points at, and the
// lifetime of the secrets it issues, the test controls while the client is
// running. Issued client ids carry the server's prefix, so a record issued by
// one server is distinguishable from another's.
type lifetimeAS struct {
	srv                *httptest.Server
	prefix             string
	discoveries        atomic.Int64
	registrations      atomic.Int64
	tokenRequests      atomic.Int64
	tokenForms         chan url.Values
	documentsSupported atomic.Bool
	secretExpiresAt    atomic.Int64 // epoch seconds; 0 means the secret never expires
	registrationAt     atomic.Value // string; empty means this server's own /register
}

func newLifetimeAS(t *testing.T, prefix string) *lifetimeAS {
	t.Helper()
	a := &lifetimeAS{prefix: prefix, tokenForms: make(chan url.Values, 8)}
	mux := http.NewServeMux()
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		a.discoveries.Add(1)
		writeJSON(w, map[string]any{
			"issuer":                                a.srv.URL,
			"authorization_endpoint":                a.srv.URL + "/authorize",
			"token_endpoint":                        a.srv.URL + "/token",
			"registration_endpoint":                 a.registrationEndpoint(),
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": a.documentsSupported.Load(),
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		n := a.registrations.Add(1)
		writeJSON(w, map[string]any{
			"client_id":                fmt.Sprintf("%s-client-%d", a.prefix, n),
			"client_secret":            fmt.Sprintf("%s-secret-%d", a.prefix, n),
			"client_secret_expires_at": a.secretExpiresAt.Load(),
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		n := a.tokenRequests.Add(1)
		select {
		case a.tokenForms <- r.Form:
		default:
		}
		writeJSON(w, map[string]any{
			"access_token":  fmt.Sprintf("%s-access-%d", a.prefix, n),
			"refresh_token": fmt.Sprintf("%s-refresh-%d", a.prefix, n),
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	return a
}

// registrationEndpoint is the endpoint this server currently advertises, which
// a test can point at another server.
func (a *lifetimeAS) registrationEndpoint() string {
	if endpoint, _ := a.registrationAt.Load().(string); endpoint != "" {
		return endpoint
	}
	return a.srv.URL + "/register"
}

// atClock points a client's clock at a variable the test advances.
func atClock(c *Client, now *time.Time) {
	c.now = func() time.Time { return *now }
}

// clientIDAt drives one authorization start at the given moment and returns the
// client id the flow resolved.
func clientIDAt(t *testing.T, c *Client, now *time.Time, advance time.Duration) string {
	t.Helper()
	*now = now.Add(advance)
	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url at %s: %v", *now, err)
	}
	return pending.ClientID
}

func TestDiscoveryIsRefreshedAfterItsCacheExpires(t *testing.T) {
	as := newLifetimeAS(t, "dyn")
	c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	// The steps are cumulative: each one advances the shared clock and observes
	// the fetches every earlier step made, so they run in order rather than as
	// independent subtests.
	steps := []struct {
		name            string
		advance         time.Duration
		wantDiscoveries int64
	}{
		{name: "first start discovers", wantDiscoveries: 1},
		{name: "within the cache window reuses", advance: discoveryTTL - time.Second, wantDiscoveries: 1},
		{name: "at the cache window rediscovers", advance: time.Second, wantDiscoveries: 2},
		{name: "well past the window rediscovers", advance: discoveryTTL * 2, wantDiscoveries: 3},
	}

	for _, step := range steps {
		clientIDAt(t, c, &now, step.advance)
		if got := as.discoveries.Load(); got != step.wantDiscoveries {
			t.Fatalf("%s: metadata fetches = %d, want %d", step.name, got, step.wantDiscoveries)
		}
	}
}

func TestRegistrationIsReusedUntilItsSecretExpires(t *testing.T) {
	as := newLifetimeAS(t, "dyn")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// The secret expires well inside registrationTTL, so the expiry is the only
	// rule that can retire the record during this test.
	as.secretExpiresAt.Store(now.Add(10 * time.Minute).Unix())

	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")
	atClock(c, &now)

	steps := []struct {
		name              string
		advance           time.Duration
		wantClientID      string
		wantRegistrations int64
	}{
		{name: "first start registers", wantClientID: "dyn-client-1", wantRegistrations: 1},
		{name: "a live registration is reused", advance: 5 * time.Minute, wantClientID: "dyn-client-1", wantRegistrations: 1},
		{name: "an expired secret is replaced", advance: 6 * time.Minute, wantClientID: "dyn-client-2", wantRegistrations: 2},
	}

	for _, step := range steps {
		if got := clientIDAt(t, c, &now, step.advance); got != step.wantClientID {
			t.Fatalf("%s: client id = %q, want %q", step.name, got, step.wantClientID)
		}
		if got := as.registrations.Load(); got != step.wantRegistrations {
			t.Fatalf("%s: registration requests = %d, want %d", step.name, got, step.wantRegistrations)
		}
	}
}

func TestRegistrationWithoutAnExpiryIsReplacedAfterItsMaximumAge(t *testing.T) {
	// A client_secret_expires_at of 0 promises only that the secret never
	// expires. It says nothing about the record surviving, and a server that has
	// dropped it answers an authorization request with an invalid_client error
	// the application never sees, so the identity is not reused indefinitely.
	as := newLifetimeAS(t, "dyn") // secretExpiresAt stays 0
	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	steps := []struct {
		name              string
		advance           time.Duration
		wantClientID      string
		wantRegistrations int64
	}{
		{name: "first start registers", wantClientID: "dyn-client-1", wantRegistrations: 1},
		{name: "reused just inside the maximum age", advance: registrationTTL - time.Second, wantClientID: "dyn-client-1", wantRegistrations: 1},
		{name: "replaced at the maximum age", advance: time.Second, wantClientID: "dyn-client-2", wantRegistrations: 2},
		{name: "the replacement is reused in turn", advance: time.Minute, wantClientID: "dyn-client-2", wantRegistrations: 2},
	}

	for _, step := range steps {
		if got := clientIDAt(t, c, &now, step.advance); got != step.wantClientID {
			t.Fatalf("%s: client id = %q, want %q", step.name, got, step.wantClientID)
		}
		if got := as.registrations.Load(); got != step.wantRegistrations {
			t.Fatalf("%s: registration requests = %d, want %d", step.name, got, step.wantRegistrations)
		}
	}
}

func TestRegistrationIsDroppedWhenTheRegistrationEndpointMoves(t *testing.T) {
	// A client record only exists on the server that issued it. Once discovery
	// reports another registration endpoint, the memoized identity belongs to a
	// server this client is no longer talking to and must not be presented.
	as := newLifetimeAS(t, "dyn")
	other := newLifetimeAS(t, "alt")
	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	if got := clientIDAt(t, c, &now, 0); got != "dyn-client-1" {
		t.Fatalf("client id = %q, want dyn-client-1", got)
	}

	as.registrationAt.Store(other.srv.URL + "/register")
	if got := clientIDAt(t, c, &now, discoveryTTL+time.Second); got != "alt-client-1" {
		t.Fatalf("client id after the endpoint moved = %q, want alt-client-1", got)
	}
	// Reuse resumes against the new endpoint rather than registering per flow.
	if got := clientIDAt(t, c, &now, time.Minute); got != "alt-client-1" {
		t.Fatalf("client id = %q, want the reused alt-client-1", got)
	}
	if got := as.registrations.Load(); got != 1 {
		t.Fatalf("original registration requests = %d, want 1", got)
	}
	if got := other.registrations.Load(); got != 1 {
		t.Fatalf("moved registration requests = %d, want 1", got)
	}
}

func TestRegistrationIsDroppedWhenTheIssuerChanges(t *testing.T) {
	// The protected resource points at a different authorization server. The
	// record registered with the first one is meaningless to the second.
	first := newLifetimeAS(t, "dyn")
	second := newLifetimeAS(t, "alt")

	var authServer atomic.Value
	authServer.Store(first.srv.URL)
	var resource *httptest.Server
	resource = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			http.NotFound(w, r)
			return
		}
		issuer, _ := authServer.Load().(string)
		writeJSON(w, map[string]any{"resource": resource.URL, "authorization_servers": []string{issuer}})
	}))
	t.Cleanup(resource.Close)

	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, resource.URL, "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	if got := clientIDAt(t, c, &now, 0); got != "dyn-client-1" {
		t.Fatalf("client id = %q, want dyn-client-1", got)
	}

	authServer.Store(second.srv.URL)
	if got := clientIDAt(t, c, &now, discoveryTTL+time.Second); got != "alt-client-1" {
		t.Fatalf("client id after the issuer changed = %q, want alt-client-1", got)
	}
	if got := first.registrations.Load(); got != 1 {
		t.Fatalf("first server registration requests = %d, want 1", got)
	}
	if got := second.registrations.Load(); got != 1 {
		t.Fatalf("second server registration requests = %d, want 1", got)
	}
}

func TestClientIDMetadataDocumentOutranksAnEarlierRegistration(t *testing.T) {
	// A server that starts advertising client ID metadata document support must
	// take the client off dynamic registration: an identity issued earlier can
	// never outrank the document.
	const documentURL = "https://app.example.com/mcp/oauth/github/client-metadata.json"
	as := newLifetimeAS(t, "dyn")
	c := NewClient(Config{
		RedirectURI:         "https://app.example.com/callback",
		ClientIDMetadataURL: documentURL,
	}, as.srv.URL+"/mcp", "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	if pending.ClientID != "dyn-client-1" || pending.ClientSecret != "dyn-secret-1" {
		t.Fatalf("first flow credentials = %q / %q, want the dynamic registration",
			pending.ClientID, pending.ClientSecret)
	}

	as.documentsSupported.Store(true)
	now = now.Add(discoveryTTL + time.Second)

	_, pending, err = c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url after the server advertised documents: %v", err)
	}
	if pending.ClientID != documentURL {
		t.Fatalf("client id = %q, want the metadata document %q", pending.ClientID, documentURL)
	}
	if pending.ClientSecret != "" {
		t.Fatalf("a metadata document client is public, but a secret was carried: %q", pending.ClientSecret)
	}
	if pending.TokenAuthMethod != authMethodNone {
		t.Fatalf("token auth method = %q, want %q", pending.TokenAuthMethod, authMethodNone)
	}
	if got := as.registrations.Load(); got != 1 {
		t.Fatalf("registration requests = %d, want 1; the document must not trigger another", got)
	}
}
