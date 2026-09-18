package mcpclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// registeringAS is a fake authorization server that counts dynamic client
// registrations, records the redirect URI each issued client was created for,
// and can advertise support for client ID metadata documents.
type registeringAS struct {
	srv        *httptest.Server
	registered atomic.Int64

	mu           sync.Mutex
	redirectURIs map[string]string // issued client id -> its registered redirect_uri
}

func newRegisteringAS(t *testing.T, documentsSupported bool) *registeringAS {
	t.Helper()
	a := &registeringAS{redirectURIs: map[string]string{}}
	mux := http.NewServeMux()
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                a.srv.URL,
			"authorization_endpoint":                a.srv.URL + "/authorize",
			"token_endpoint":                        a.srv.URL + "/token",
			"registration_endpoint":                 a.srv.URL + "/register",
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": documentsSupported,
		})
	})
	// Every registration issues a distinct client id, so a reused identity is
	// distinguishable from a freshly created client record.
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		clientID := fmt.Sprintf("dyn-client-%d", a.registered.Add(1))
		a.mu.Lock()
		if len(body.RedirectURIs) > 0 {
			a.redirectURIs[clientID] = body.RedirectURIs[0]
		}
		a.mu.Unlock()

		writeJSON(w, map[string]any{
			"client_id":     clientID,
			"client_secret": "dyn-secret",
		})
	})
	return a
}

// registeredFor returns the redirect URI an issued client id was created for.
func (a *registeringAS) registeredFor(clientID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.redirectURIs[clientID]
}

// startFlow drives one redirect leg through the mounted router and returns the
// query of the authorization URL the module sent the browser to.
func startFlow(t *testing.T, call func(method, target string) *httptest.ResponseRecorder, origin, path string) url.Values {
	t.Helper()
	return startFlowURL(t, call, origin, path).Query()
}

// startFlowURL drives one redirect leg and returns the whole authorization URL,
// for tests that care which authorization server it points at.
func startFlowURL(t *testing.T, call func(method, target string) *httptest.ResponseRecorder, origin, path string) *url.URL {
	t.Helper()
	rec := call(http.MethodGet, origin+path)
	if rec.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302; body: %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return u
}

func TestRedirectReusesTheIssuedRegistration(t *testing.T) {
	as := newRegisteringAS(t, false)
	RegisterClient("reuse", as.srv.URL+"/mcp")
	p := OAuthRoutesFor("reuse", oauth.Config{},
		WithStore(NewMemoryStore()),
		WithPublicURL("https://app.example.com"))
	call := mountModule(t, p)

	const callbackURI = "https://app.example.com/mcp/oauth/reuse/callback"
	first := startFlow(t, call, "https://app.example.com", "/mcp/oauth/reuse/redirect")
	// A later request arriving with a different Host must not change the
	// identity or the redirect URI: both come from the pinned public URL.
	second := startFlow(t, call, "http://spoofed.example.net", "/mcp/oauth/reuse/redirect")

	if got := first.Get("client_id"); got != "dyn-client-1" {
		t.Fatalf("first client_id = %q, want dyn-client-1", got)
	}
	if got := second.Get("client_id"); got != "dyn-client-1" {
		t.Fatalf("second client_id = %q, want the reused registration dyn-client-1", got)
	}
	for i, query := range []url.Values{first, second} {
		if got := query.Get("redirect_uri"); got != callbackURI {
			t.Fatalf("flow %d redirect_uri = %q, want %q", i, got, callbackURI)
		}
	}
	if n := as.registered.Load(); n != 1 {
		t.Fatalf("registration requests = %d, want 1; each connection registered a new client", n)
	}
	if got := as.registeredFor("dyn-client-1"); got != callbackURI {
		t.Fatalf("dyn-client-1 was registered for %q, want %q", got, callbackURI)
	}
}

func TestRedirectDoesNotReuseARegistrationAcrossOrigins(t *testing.T) {
	// Without a pinned public URL the redirect URI follows the Host header. A
	// registration is bound to the redirect_uri it was created for, so one
	// request carrying an unexpected Host must not decide the identity every
	// later flow uses: an authorization server rejects a client_id presented
	// with a redirect_uri it was not registered for.
	as := newRegisteringAS(t, false)
	RegisterClient("origins", as.srv.URL+"/mcp")
	p := OAuthRoutesFor("origins", oauth.Config{}, WithStore(NewMemoryStore()))
	call := mountModule(t, p)

	unexpected := startFlow(t, call, "http://evil.example.net", "/mcp/oauth/origins/redirect")
	legitimate := startFlow(t, call, "http://legit.example.com", "/mcp/oauth/origins/redirect")

	if n := as.registered.Load(); n != 2 {
		t.Fatalf("registration requests = %d, want 2; one per origin", n)
	}
	if unexpected.Get("client_id") == legitimate.Get("client_id") {
		t.Fatalf("both origins used client_id %q; a registration leaked across origins", legitimate.Get("client_id"))
	}
	for _, tt := range []struct {
		origin string
		query  url.Values
	}{
		{origin: "http://evil.example.net", query: unexpected},
		{origin: "http://legit.example.com", query: legitimate},
	} {
		want := tt.origin + "/mcp/oauth/origins/callback"
		if got := tt.query.Get("redirect_uri"); got != want {
			t.Fatalf("%s redirect_uri = %q, want %q", tt.origin, got, want)
		}
		if got := as.registeredFor(tt.query.Get("client_id")); got != want {
			t.Fatalf("%s used client_id %q, registered for %q, want %q",
				tt.origin, tt.query.Get("client_id"), got, want)
		}
	}
}

func TestRedirectPrefersTheClientIDMetadataDocument(t *testing.T) {
	as := newRegisteringAS(t, true)
	RegisterClient("cimd", as.srv.URL+"/mcp")
	p := OAuthRoutesFor("cimd", oauth.Config{},
		WithStore(NewMemoryStore()),
		WithPublicURL("https://app.example.com"))
	call := mountModule(t, p)

	const documentURL = "https://app.example.com/mcp/oauth/cimd/client-metadata.json"
	// The client_id handed to the authorization server is the document the
	// pinned public URL publishes, whatever origin the browser reached this
	// application on. Behind a proxy the Host is an internal name, and an
	// internal document URL is not a client identifier any authorization server
	// could dereference; a spoofed Host is worse, because it would let one
	// request choose the identity the server is told.
	// The very first flow arrives on the internal origin, because it is the one
	// that would otherwise decide the identity every later flow presents.
	flows := []struct {
		name   string
		origin string
	}{
		{name: "an internal origin behind a proxy", origin: "http://localhost:4000"},
		{name: "a spoofed host", origin: "http://spoofed.example.net"},
		{name: "the public origin", origin: "https://app.example.com"},
	}

	for _, flow := range flows {
		query := startFlow(t, call, flow.origin, "/mcp/oauth/cimd/redirect")
		if got := query.Get("client_id"); got != documentURL {
			t.Fatalf("a flow from %s sent client_id = %q, want the pinned document %q", flow.name, got, documentURL)
		}
		if got := query.Get("redirect_uri"); got != "https://app.example.com/mcp/oauth/cimd/callback" {
			t.Fatalf("a flow from %s sent redirect_uri = %q", flow.name, got)
		}
	}
	// The document identifies the client, so no client record is ever created
	// and nothing a registration would have issued can outrank the document.
	if n := as.registered.Load(); n != 0 {
		t.Fatalf("registration requests = %d, want 0; the metadata document should identify the client", n)
	}
}

func TestRedirectWithoutAPinnedPublicURLCannotUseTheDocument(t *testing.T) {
	// Without a pinned public URL the document URL is built from the request, so
	// a development origin yields "http://localhost:4000/...": not https, not
	// resolvable from outside this machine, and therefore not a client_id an
	// authorization server could fetch. The flow registers dynamically instead
	// of presenting an identifier that would be rejected.
	as := newRegisteringAS(t, true)
	RegisterClient("unpinned-cimd", as.srv.URL+"/mcp")
	p := OAuthRoutesFor("unpinned-cimd", oauth.Config{}, WithStore(NewMemoryStore()))
	call := mountModule(t, p)

	query := startFlow(t, call, "http://localhost:4000", "/mcp/oauth/unpinned-cimd/redirect")

	if got := query.Get("client_id"); got != "dyn-client-1" {
		t.Fatalf("client_id = %q, want the dynamically registered dyn-client-1", got)
	}
	if n := as.registered.Load(); n != 1 {
		t.Fatalf("registration requests = %d, want 1; an unusable document URL must fall back to registration", n)
	}
	if got := as.registeredFor("dyn-client-1"); got != "http://localhost:4000/mcp/oauth/unpinned-cimd/callback" {
		t.Fatalf("dyn-client-1 was registered for %q", got)
	}
}

func TestRedirectDoesNotReuseAClientAcrossResourceServers(t *testing.T) {
	// One OAuth client is shared between flows only while it addresses the same
	// resource server and redirect URI: it carries the metadata it discovered and
	// the record it registered there. Pointing the registered name at another
	// server must discard it, or the second server would be handed the first
	// server's client_id.
	first := newRegisteringAS(t, false)
	RegisterClient("moved", first.srv.URL+"/mcp")
	p := OAuthRoutesFor("moved", oauth.Config{},
		WithStore(NewMemoryStore()),
		WithPublicURL("https://app.example.com"))
	call := mountModule(t, p)

	before := startFlowURL(t, call, "https://app.example.com", "/mcp/oauth/moved/redirect")
	if got := endpointOrigin(before); got != first.srv.URL {
		t.Fatalf("first flow authorized against %q, want %q", got, first.srv.URL)
	}

	second := newRegisteringAS(t, false)
	RegisterClient("moved", second.srv.URL+"/mcp")
	after := startFlowURL(t, call, "https://app.example.com", "/mcp/oauth/moved/redirect")

	if got := endpointOrigin(after); got != second.srv.URL {
		t.Fatalf("second flow authorized against %q, want the re-registered %q", got, second.srv.URL)
	}
	if n := second.registered.Load(); n != 1 {
		t.Fatalf("the second server issued %d registrations, want 1 of its own", n)
	}
	if n := first.registered.Load(); n != 1 {
		t.Fatalf("the first server issued %d registrations, want 1", n)
	}
	const callbackURI = "https://app.example.com/mcp/oauth/moved/callback"
	clientID := after.Query().Get("client_id")
	if got := second.registeredFor(clientID); got != callbackURI {
		t.Fatalf("the second server registered %q for %q, want %q", clientID, got, callbackURI)
	}
}

func TestConcurrentRedirectsRegisterOnce(t *testing.T) {
	as := newRegisteringAS(t, false)
	RegisterClient("concurrent", as.srv.URL+"/mcp")
	p := OAuthRoutesFor("concurrent", oauth.Config{},
		WithStore(NewMemoryStore()),
		WithPublicURL("https://app.example.com"))
	call := mountModule(t, p)
	// Commit the routes before the burst so the race is over the registration,
	// not over first-request route compilation.
	call(http.MethodGet, "https://app.example.com/mcp/oauth/concurrent/client-metadata.json")

	queries := concurrentFlows(t, call, "https://app.example.com", "/mcp/oauth/concurrent/redirect", 8)

	if n := as.registered.Load(); n != 1 {
		t.Fatalf("registration requests = %d, want exactly 1; the burst was not single-flighted", n)
	}
	states := map[string]bool{}
	for i, query := range queries {
		if got := query.Get("client_id"); got != "dyn-client-1" {
			t.Fatalf("flow %d client_id = %q, want the single registration dyn-client-1", i, got)
		}
		state := query.Get("state")
		if state == "" || states[state] {
			t.Fatalf("flow %d state = %q, want a fresh anti-CSRF state per flow", i, state)
		}
		states[state] = true
	}
}

func TestConcurrentRedirectsWithoutAPinnedPublicURLRegisterPerFlow(t *testing.T) {
	// Nothing is shared when the redirect URI comes from the request, so each
	// flow registers for itself. That is the cost of leaving WithPublicURL
	// unset, and it is bounded: one client record per authorization start, none
	// of them usable with another origin's redirect URI.
	as := newRegisteringAS(t, false)
	RegisterClient("unpinned", as.srv.URL+"/mcp")
	p := OAuthRoutesFor("unpinned", oauth.Config{}, WithStore(NewMemoryStore()))
	call := mountModule(t, p)
	call(http.MethodGet, "http://localhost:4000/mcp/oauth/unpinned/client-metadata.json")

	const flows = 6
	queries := concurrentFlows(t, call, "http://localhost:4000", "/mcp/oauth/unpinned/redirect", flows)

	if n := as.registered.Load(); n != flows {
		t.Fatalf("registration requests = %d, want %d, one per flow", n, flows)
	}
	issued := map[string]bool{}
	for i, query := range queries {
		id := query.Get("client_id")
		if id == "" || issued[id] {
			t.Fatalf("flow %d client_id = %q, want a distinct registration per flow", i, id)
		}
		issued[id] = true
		if got := as.registeredFor(id); got != "http://localhost:4000/mcp/oauth/unpinned/callback" {
			t.Fatalf("flow %d registered for %q", i, got)
		}
	}
}

// endpointOrigin is the scheme://host an authorization URL points at.
func endpointOrigin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// concurrentFlows drives n redirect legs at once and returns each resulting
// authorization URL query.
func concurrentFlows(t *testing.T, call func(method, target string) *httptest.ResponseRecorder, origin, path string, n int) []url.Values {
	t.Helper()
	var wg sync.WaitGroup
	results := make([]url.Values, n)
	failures := make([]string, n)

	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			rec := call(http.MethodGet, origin+path)
			if rec.Code != http.StatusFound {
				failures[i] = fmt.Sprintf("status %d: %s", rec.Code, rec.Body.String())
				return
			}
			u, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				failures[i] = "bad location: " + err.Error()
				return
			}
			results[i] = u.Query()
		}()
	}
	wg.Wait()

	for i, failure := range failures {
		if failure != "" {
			t.Fatalf("concurrent flow %d: %s", i, failure)
		}
	}
	return results
}
