package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// movableResource is a protected resource whose advertised authorization server
// a test moves while the client is running, which is what a compromised or
// hijacked resource does to point a client at a server of its choosing.
type movableResource struct {
	srv    *httptest.Server
	issuer atomic.Value // string
}

func newMovableResource(t *testing.T, issuer string) *movableResource {
	t.Helper()
	r := &movableResource{}
	r.issuer.Store(issuer)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/.well-known/oauth-protected-resource" {
			http.NotFound(w, req)
			return
		}
		current, _ := r.issuer.Load().(string)
		writeJSON(w, map[string]any{"resource": r.srv.URL, "authorization_servers": []string{current}})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// moveTo points the resource at another authorization server.
func (r *movableResource) moveTo(issuer string) { r.issuer.Store(issuer) }

// authorizedTokenSet drives a full authorization-code flow against the resource
// and returns the issued token set, which carries the credentials the flow
// registered dynamically.
func authorizedTokenSet(t *testing.T, c *Client) *TokenSet {
	t.Helper()
	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	token, _, err := c.ExchangeCode(context.Background(), pending, url.Values{
		"code":  {"the-code"},
		"state": {pending.State},
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	return token
}

func TestRefreshIsRefusedWhenTheResourceMovesToAnotherAuthorizationServer(t *testing.T) {
	// The resource swaps its authorization server once the discovery cache has
	// expired. The refresh token and the client secret the first server issued
	// are only meaningful to that server, so neither may be sent to the second.
	first := newLifetimeAS(t, "dyn")
	second := newLifetimeAS(t, "alt")
	resource := newMovableResource(t, first.srv.URL)

	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, resource.srv.URL, "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	token := authorizedTokenSet(t, c)
	if token.Issuer != first.srv.URL {
		t.Fatalf("token issuer = %q, want %q", token.Issuer, first.srv.URL)
	}
	if token.ClientSecret != "dyn-secret-1" || token.RefreshToken != "dyn-refresh-1" {
		t.Fatalf("token = %+v, want the first server's secret and refresh token", token)
	}

	resource.moveTo(second.srv.URL)
	now = now.Add(discoveryTTL + time.Second)

	refreshed, err := c.Refresh(context.Background(), token)
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("refresh error = %v, want one wrapping ErrIssuerMismatch", err)
	}
	if refreshed != nil {
		t.Fatalf("a token was returned for a refused refresh: %+v", refreshed)
	}
	if got := second.tokenRequests.Load(); got != 0 {
		t.Fatalf("the new authorization server received %d token requests, want 0", got)
	}
	if got := first.tokenRequests.Load(); got != 1 {
		t.Fatalf("the original authorization server received %d token requests, want the exchange only", got)
	}
}

func TestRefreshIsRefusedForTokenSetsThatCannotBeBoundToTheCurrentServer(t *testing.T) {
	as := newLifetimeAS(t, "dyn")

	tests := []struct {
		name  string
		token func(issuer string) *TokenSet
	}{
		{
			name:  "no issuer recorded",
			token: func(string) *TokenSet { return &TokenSet{RefreshToken: "r", ClientID: "cid"} },
		},
		{
			name: "another issuer recorded",
			token: func(string) *TokenSet {
				return &TokenSet{RefreshToken: "r", ClientID: "cid", Issuer: "https://evil.example.com"}
			},
		},
		{
			name: "issuer recorded as a prefix of the current one",
			token: func(issuer string) *TokenSet {
				return &TokenSet{RefreshToken: "r", ClientID: "cid", Issuer: issuer[:len(issuer)-1]}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL}, as.srv.URL+"/mcp", "", "")
			before := as.tokenRequests.Load()

			token, err := c.Refresh(context.Background(), tt.token(as.srv.URL))
			if !errors.Is(err, ErrIssuerMismatch) {
				t.Fatalf("refresh error = %v, want one wrapping ErrIssuerMismatch", err)
			}
			if token != nil {
				t.Fatalf("a token was returned for a refused refresh: %+v", token)
			}
			if got := as.tokenRequests.Load(); got != before {
				t.Fatalf("token requests = %d, want %d; the refresh token left the process", got, before)
			}
		})
	}
}

func TestRefreshRequiresATokenSetCarryingARefreshToken(t *testing.T) {
	// Refresh takes the issued set rather than loose strings, so the binding
	// cannot be left out by accident. A set that carries nothing to refresh is
	// reported as such, before discovery and before any request, and it is
	// reported as its own condition rather than as a mismatch, since a caller
	// answers it by authorizing afresh rather than by re-registering.
	tests := []struct {
		name  string
		token func(issuer string) *TokenSet
	}{
		{
			name:  "no token set at all",
			token: func(string) *TokenSet { return nil },
		},
		{
			name:  "a set with no refresh token",
			token: func(issuer string) *TokenSet { return &TokenSet{AccessToken: "a", Issuer: issuer} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newLifetimeAS(t, "dyn")
			c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL}, as.srv.URL+"/mcp", "", "")

			token, err := c.Refresh(context.Background(), tt.token(as.srv.URL))
			if !errors.Is(err, ErrNoRefreshToken) {
				t.Fatalf("refresh error = %v, want one wrapping ErrNoRefreshToken", err)
			}
			if errors.Is(err, ErrIssuerMismatch) {
				t.Fatalf("error = %v, want a missing-input refusal rather than a mismatch", err)
			}
			if token != nil {
				t.Fatalf("a token was returned for a refused refresh: %+v", token)
			}
			if got := as.discoveries.Load(); got != 0 {
				t.Fatalf("metadata fetches = %d, want 0; the refusal is not worth a round trip", got)
			}
			if got := as.tokenRequests.Load(); got != 0 {
				t.Fatalf("token requests = %d, want 0", got)
			}
		})
	}
}

func TestIssuedTokenSetsRecordTheirAuthorizationServer(t *testing.T) {
	// Every grant stamps the server that issued the set, so the set can be bound
	// on a later refresh.
	tests := []struct {
		name  string
		issue func(t *testing.T, c *Client, issuer string) *TokenSet
	}{
		{
			name:  "authorization code",
			issue: func(t *testing.T, c *Client, _ string) *TokenSet { return authorizedTokenSet(t, c) },
		},
		{
			name: "client credentials",
			issue: func(t *testing.T, c *Client, _ string) *TokenSet {
				token, err := c.ClientCredentials(context.Background())
				if err != nil {
					t.Fatalf("client credentials: %v", err)
				}
				return token
			},
		},
		{
			name: "refresh",
			issue: func(t *testing.T, c *Client, issuer string) *TokenSet {
				token, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "r", Issuer: issuer})
				if err != nil {
					t.Fatalf("refresh: %v", err)
				}
				return token
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newLifetimeAS(t, "dyn")
			c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")

			token := tt.issue(t, c, as.srv.URL)
			if token.Issuer != as.srv.URL {
				t.Fatalf("token issuer = %q, want %q", token.Issuer, as.srv.URL)
			}
		})
	}
}

// configuredSecret is the client secret an application holds out of band, as
// opposed to one an authorization server issued to it by dynamic registration.
const configuredSecret = "configured-secret"

// movedResourceConfig is the configuration of an application whose credentials
// were issued by issuer.
func movedResourceConfig(issuer string) Config {
	return Config{
		ClientID:     "cid",
		ClientSecret: configuredSecret,
		Issuer:       issuer,
		RedirectURI:  "https://app.example.com/callback",
	}
}

// publicClientConfig is the configuration of an application that holds no client
// secret and names the authorization server it is registered with.
func publicClientConfig(issuer string) Config {
	return Config{
		ClientID:    "cid",
		Issuer:      issuer,
		RedirectURI: "https://app.example.com/callback",
	}
}

// afterTheResourceMovesConfiguredBy returns a client built from configure, after
// the protected resource it was pointed at has stopped advertising the first
// authorization server, started advertising the second, and the discovery cache
// has expired. A grant runs against the first server on the way, so a refusal
// afterwards is the move and nothing else.
func afterTheResourceMovesConfiguredBy(t *testing.T, configure func(issuer string) Config) (c *Client, first, second *lifetimeAS) {
	t.Helper()
	first = newLifetimeAS(t, "dyn")
	second = newLifetimeAS(t, "alt")
	resource := newMovableResource(t, first.srv.URL)

	c = NewClient(configure(first.srv.URL), resource.srv.URL, "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	if _, err := c.ClientCredentials(context.Background()); err != nil {
		t.Fatalf("client credentials before the move: %v", err)
	}

	resource.moveTo(second.srv.URL)
	now = now.Add(discoveryTTL + time.Second)
	return c, first, second
}

// afterTheResourceMoves is afterTheResourceMovesConfiguredBy for an application
// holding a configured client secret, which reaches the first server on the way.
func afterTheResourceMoves(t *testing.T) (c *Client, first, second *lifetimeAS) {
	t.Helper()
	c, first, second = afterTheResourceMovesConfiguredBy(t, movedResourceConfig)
	if got := (<-first.tokenForms).Get("client_secret"); got != configuredSecret {
		t.Fatalf("client_secret sent to the configured server = %q, want %q", got, configuredSecret)
	}
	return c, first, second
}

// pendingFor is a persisted authorization carrying the configured credentials
// and naming as the server it was started against, which is the shape
// ExchangeCode is handed after a callback.
func pendingFor(as *lifetimeAS) *PendingAuthorization {
	return &PendingAuthorization{
		State:           "the-state",
		Verifier:        "0123456789012345678901234567890123456789012345",
		ClientID:        "cid",
		ClientSecret:    configuredSecret,
		TokenEndpoint:   as.srv.URL + "/token",
		TokenAuthMethod: authMethodPost,
		RedirectURI:     "https://app.example.com/callback",
		Issuer:          as.srv.URL,
	}
}

// callback is the query an authorization server redirects back with for pending.
func callbackFor(pending *PendingAuthorization) url.Values {
	return url.Values{"code": {"the-code"}, "state": {pending.State}}
}

func TestTheConfiguredSecretIsNotPresentedToAnotherAuthorizationServer(t *testing.T) {
	// The configured credentials belong to the server named in the
	// configuration, whichever one the protected resource currently advertises.
	// A resource that moves collects nothing on any path that would carry them.
	tests := []struct {
		name string
		run  func(c *Client, first, second *lifetimeAS) error
	}{
		{
			name: "client credentials",
			run: func(c *Client, _, _ *lifetimeAS) error {
				_, err := c.ClientCredentials(context.Background())
				return err
			},
		},
		{
			name: "authorization start",
			run: func(c *Client, _, _ *lifetimeAS) error {
				_, _, err := c.AuthorizationURL(context.Background(), "")
				return err
			},
		},
		{
			name: "refresh of a set the first server issued",
			run: func(c *Client, first, _ *lifetimeAS) error {
				_, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "dyn-refresh-1", Issuer: first.srv.URL})
				return err
			},
		},
		{
			name: "code exchange against an authorization started for the new server",
			run: func(c *Client, _, second *lifetimeAS) error {
				pending := pendingFor(second)
				_, _, err := c.ExchangeCode(context.Background(), pending, callbackFor(pending))
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, first, second := afterTheResourceMoves(t)

			err := tt.run(c, first, second)
			if !errors.Is(err, ErrIssuerMismatch) {
				t.Fatalf("error = %v, want one wrapping ErrIssuerMismatch", err)
			}
			if got := second.tokenRequests.Load(); got != 0 {
				t.Fatalf("the new authorization server received %d token requests, want 0", got)
			}
			if got := first.tokenRequests.Load(); got != 1 {
				t.Fatalf("the configured authorization server received %d token requests, want the one before the move", got)
			}
		})
	}
}

func TestTheConfiguredSecretIsBoundForEveryClientBuiltFromTheConfiguration(t *testing.T) {
	// The binding is the configuration, not one client's memory of which server
	// it happened to meet first. An application that starts authorizations on a
	// long-lived client and completes each callback on a fresh one is bound just
	// the same, and so is the same application after a restart.
	first := newLifetimeAS(t, "dyn")
	second := newLifetimeAS(t, "alt")
	resource := newMovableResource(t, first.srv.URL)
	cfg := movedResourceConfig(first.srv.URL)

	starts := NewClient(cfg, resource.srv.URL, "", "")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(starts, &now)

	_, pending, err := starts.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	token, _, err := NewClient(cfg, resource.srv.URL, "", "").
		ExchangeCode(context.Background(), pending, callbackFor(pending))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if token.Issuer != first.srv.URL {
		t.Fatalf("token issuer = %q, want %q", token.Issuer, first.srv.URL)
	}
	if got := (<-first.tokenForms).Get("client_secret"); got != configuredSecret {
		t.Fatalf("client_secret sent to the configured server = %q, want %q", got, configuredSecret)
	}

	resource.moveTo(second.srv.URL)
	now = now.Add(discoveryTTL + time.Second)

	if _, _, err := starts.AuthorizationURL(context.Background(), ""); !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("authorization url after the move = %v, want one wrapping ErrIssuerMismatch", err)
	}
	moved := pendingFor(second)
	if _, _, err := NewClient(cfg, resource.srv.URL, "", "").
		ExchangeCode(context.Background(), moved, callbackFor(moved)); !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("exchange on a fresh client after the move = %v, want one wrapping ErrIssuerMismatch", err)
	}
	if got := second.tokenRequests.Load(); got != 0 {
		t.Fatalf("the new authorization server received %d token requests, want 0", got)
	}
}

func TestExchangeCodeIsRefusedForAnAuthorizationStartedWithAnotherServer(t *testing.T) {
	// A configured issuer pins a public client too. An authorization code and
	// the PKCE verifier that unlocks it are redeemed at the server the flow was
	// started with and at no other, whatever a stored pending authorization has
	// come to name.
	first := newLifetimeAS(t, "dyn")
	second := newLifetimeAS(t, "alt")
	c := NewClient(Config{
		ClientID:    "cid",
		Issuer:      first.srv.URL,
		RedirectURI: "https://app.example.com/callback",
	}, first.srv.URL+"/mcp", "", "")

	pending := pendingFor(second)
	pending.ClientSecret = ""
	pending.TokenAuthMethod = authMethodNone

	token, _, err := c.ExchangeCode(context.Background(), pending, callbackFor(pending))
	if !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("exchange error = %v, want one wrapping ErrIssuerMismatch", err)
	}
	if token != nil {
		t.Fatalf("a token was returned for a refused exchange: %+v", token)
	}
	if got := second.tokenRequests.Load(); got != 0 {
		t.Fatalf("the other authorization server received %d token requests, want 0", got)
	}
}

func TestAConfiguredIssuerPinsAClientThatCarriesNoSecret(t *testing.T) {
	// A configured issuer pins the whole client, not only the paths a secret
	// travels on. A public client has plenty left to lose to a server the
	// resource nominates: the authorization request walks the user to that
	// server's consent screen for this application's identity, and the
	// client-credentials grant hands it a client_id and the resource indicator.
	// Neither may leave for a server the configuration does not name.
	tests := []struct {
		name string
		// run returns whatever the flow hands its caller: the redirect for an
		// authorization start, the access token for a grant. A refused flow
		// hands back nothing.
		run func(c *Client) (string, error)
	}{
		{
			name: "authorization start",
			run: func(c *Client) (string, error) {
				authURL, _, err := c.AuthorizationURL(context.Background(), "")
				return authURL, err
			},
		},
		{
			name: "client credentials",
			run: func(c *Client) (string, error) {
				token, err := c.ClientCredentials(context.Background())
				if token == nil {
					return "", err
				}
				return token.AccessToken, err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, first, second := afterTheResourceMovesConfiguredBy(t, publicClientConfig)
			form := <-first.tokenForms
			if got := form.Get("client_secret"); got != "" {
				t.Fatalf("the grant before the move sent client_secret=%q, want none from a public client", got)
			}
			if got := form.Get("client_id"); got != "cid" {
				t.Fatalf("client_id sent before the move = %q, want %q; this client has an identity to lose", got, "cid")
			}

			out, err := tt.run(c)
			if !errors.Is(err, ErrIssuerMismatch) {
				t.Fatalf("error = %v, want one wrapping ErrIssuerMismatch", err)
			}
			if out != "" {
				t.Fatalf("a refused flow handed back %q, want nothing", out)
			}
			if got := second.tokenRequests.Load(); got != 0 {
				t.Fatalf("the new authorization server received %d token requests, want 0", got)
			}
			if got := first.tokenRequests.Load(); got != 1 {
				t.Fatalf("the configured authorization server received %d token requests, want the one before the move", got)
			}
		})
	}
}

func TestConfiguredCredentialsCannotBePresentedWithoutTheirIssuer(t *testing.T) {
	// Credentials an application holds out of band say nothing about the server
	// that issued them, and the protected resource that would name one is the
	// party this binding has to hold against. An unconfigured issuer is
	// therefore refused rather than taken from whichever server answered first.
	//
	// That holds for a public client as much as for one with a secret. The MCP
	// authorization specification has pre-registered credentials keyed by the
	// issuer they were registered with, and a client id is such a credential: it
	// exists at one authorization server, and shown to another it puts this
	// application's name on a consent screen the application never set up.
	clients := []struct {
		name      string
		configure func(issuer string) Config
		// pending adapts a persisted authorization to the client's credentials.
		pending func(p *PendingAuthorization)
	}{
		{
			name:      "a client with a secret",
			configure: movedResourceConfig,
			pending:   func(*PendingAuthorization) {},
		},
		{
			name:      "a public client",
			configure: publicClientConfig,
			pending: func(p *PendingAuthorization) {
				p.ClientSecret = ""
				p.TokenAuthMethod = authMethodNone
			},
		},
	}
	flows := []struct {
		name string
		run  func(c *Client, pending *PendingAuthorization, as *lifetimeAS) error
	}{
		{
			name: "authorization start",
			run: func(c *Client, _ *PendingAuthorization, _ *lifetimeAS) error {
				authURL, pending, err := c.AuthorizationURL(context.Background(), "")
				if authURL != "" || pending != nil {
					return fmt.Errorf("a refused start handed back %q and %+v", authURL, pending)
				}
				return err
			},
		},
		{
			name: "client credentials",
			run: func(c *Client, _ *PendingAuthorization, _ *lifetimeAS) error {
				_, err := c.ClientCredentials(context.Background())
				return err
			},
		},
		{
			name: "refresh",
			run: func(c *Client, _ *PendingAuthorization, as *lifetimeAS) error {
				_, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "r", Issuer: as.srv.URL})
				return err
			},
		},
		{
			name: "refresh of a set that names the configured client",
			run: func(c *Client, _ *PendingAuthorization, as *lifetimeAS) error {
				_, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "r", ClientID: "cid", Issuer: as.srv.URL})
				return err
			},
		},
		{
			name: "code exchange",
			run: func(c *Client, pending *PendingAuthorization, _ *lifetimeAS) error {
				_, _, err := c.ExchangeCode(context.Background(), pending, callbackFor(pending))
				return err
			},
		},
	}

	for _, client := range clients {
		for _, flow := range flows {
			t.Run(client.name+"/"+flow.name, func(t *testing.T) {
				as := newLifetimeAS(t, "dyn")
				c := NewClient(client.configure(""), as.srv.URL+"/mcp", "", "")
				pending := pendingFor(as)
				client.pending(pending)

				err := flow.run(c, pending, as)
				if !errors.Is(err, ErrIssuerRequired) {
					t.Fatalf("error = %v, want one wrapping ErrIssuerRequired", err)
				}
				if errors.Is(err, ErrIssuerMismatch) {
					t.Fatalf("error = %v, want a missing-configuration refusal rather than a mismatch", err)
				}
				if got := as.tokenRequests.Load(); got != 0 {
					t.Fatalf("token requests = %d, want 0", got)
				}
			})
		}
	}
}

func TestOnlyConfiguredCredentialsNeedAConfiguredIssuer(t *testing.T) {
	// The issuer requirement follows the credentials held in Config and nothing
	// else. A client record issued by dynamic registration is bound by the
	// client that registered it, and a token set carrying such a record brings
	// its own issuer, so neither asks the configuration for one.
	tests := []struct {
		name   string
		config Config
		run    func(c *Client, as *lifetimeAS) (clientID string, err error)
	}{
		{
			name:   "a flow that registers dynamically",
			config: Config{RedirectURI: "https://app.example.com/callback"},
			run: func(c *Client, _ *lifetimeAS) (string, error) {
				_, pending, err := c.AuthorizationURL(context.Background(), "")
				if err != nil {
					return "", err
				}
				return pending.ClientID, nil
			},
		},
		{
			name:   "a refresh presenting the registered client a token set carries",
			config: Config{RedirectURI: "https://app.example.com/callback"},
			run: func(c *Client, as *lifetimeAS) (string, error) {
				token, err := c.Refresh(context.Background(), &TokenSet{
					RefreshToken: "r", ClientID: "dyn-client-7", ClientSecret: "dyn-secret-7", Issuer: as.srv.URL,
				})
				if err != nil {
					return "", err
				}
				return token.ClientID, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newLifetimeAS(t, "dyn")
			c := NewClient(tt.config, as.srv.URL+"/mcp", "", "")

			clientID, err := tt.run(c, as)
			if err != nil {
				t.Fatalf("a flow carrying no configured credentials was refused: %v", err)
			}
			if !strings.HasPrefix(clientID, "dyn-client-") {
				t.Fatalf("client id = %q, want a dynamically registered one", clientID)
			}
		})
	}
}

func TestConcurrentFlowsAreAllRefusedAfterTheResourceMoves(t *testing.T) {
	// Discovery is shared between the flows of one client, so the refusal has to
	// hold for every flow that reads it, not only the one that fetched it. The
	// race detector covers that sharing at the same time.
	c, first, second := afterTheResourceMoves(t)

	const flows = 8
	grants := []func() error{
		func() error {
			_, err := c.ClientCredentials(context.Background())
			return err
		},
		func() error {
			_, _, err := c.AuthorizationURL(context.Background(), "")
			return err
		},
		func() error {
			_, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "dyn-refresh-1", Issuer: first.srv.URL})
			return err
		},
		func() error {
			pending := pendingFor(second)
			_, _, err := c.ExchangeCode(context.Background(), pending, callbackFor(pending))
			return err
		},
	}

	var wg sync.WaitGroup
	errs := make(chan error, flows)
	for i := range flows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- grants[i%len(grants)]()
		}()
	}
	wg.Wait()
	close(errs)

	refused := 0
	for err := range errs {
		if !errors.Is(err, ErrIssuerMismatch) {
			t.Fatalf("concurrent flow error = %v, want one wrapping ErrIssuerMismatch", err)
		}
		refused++
	}
	if refused != flows {
		t.Fatalf("refused %d flows, want %d", refused, flows)
	}
	if got := second.tokenRequests.Load(); got != 0 {
		t.Fatalf("the new authorization server received %d token requests, want 0", got)
	}
}
