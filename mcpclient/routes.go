package mcpclient

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client"
	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// DefaultBasePath is the path prefix the OAuth client routes mount under.
const DefaultBasePath = "/mcp/oauth"

// flowTimeout bounds the discovery + token exchange network work per request.
const flowTimeout = 15 * time.Second

// OAuthRouteModule mounts the authorization-code routes for one registered
// MCP server. It implements chain.RouteModule (so it installs when added to
// the module registry) and app.Module (so it is addable via
// reg.Add).
type OAuthRouteModule struct {
	name     string
	cfg      oauth.Config
	store    Store
	success  string
	basePath string

	clientMetadata map[string]any
	metadataURI    string
	publicURL      string

	// mu guards the OAuth client shared between flows, which exists only when
	// WithPublicURL pinned the redirect URI. It is application-wide state, not
	// per-browser state.
	mu        sync.Mutex
	shared    *oauth.Client
	sharedFor string
}

// Option customises an OAuthRouteModule.
type Option func(*OAuthRouteModule)

// WithStore overrides the token/pending store (default: the package default
// store, SessionStore).
func WithStore(s Store) Option { return func(p *OAuthRouteModule) { p.store = s } }

// WithSuccessRedirect sets the path to redirect to after a successful exchange
// when the flow carried no return target (default "/").
func WithSuccessRedirect(path string) Option {
	return func(p *OAuthRouteModule) { p.success = path }
}

// WithBasePath overrides the route prefix (default DefaultBasePath).
func WithBasePath(prefix string) Option {
	return func(p *OAuthRouteModule) {
		if prefix != "" {
			p.basePath = prefix
		}
	}
}

// WithClientMetadata adds descriptive fields to the published client ID
// metadata document, for example client_name, logo_uri, client_uri, or extra
// redirect_uris declared alongside the callback this module serves. Credential
// fields are dropped and the computed client_id, redirect_uris and
// token_endpoint_auth_method cannot be overridden.
func WithClientMetadata(metadata map[string]any) Option {
	return func(p *OAuthRouteModule) {
		p.clientMetadata = maps.Clone(metadata)
	}
}

// WithClientMetadataPath publishes the client ID metadata document at a path of
// your choosing instead of "{base}/{name}/client-metadata.json".
func WithClientMetadataPath(path string) Option {
	return func(p *OAuthRouteModule) { p.metadataURI = normalizePath(path) }
}

// WithPublicURL pins the absolute base URL this application is reachable at
// (for example "https://app.example.com"). The redirect URI, the published
// client ID metadata document and the client_id inside it are built from it
// instead of from the incoming request, so a spoofed Host header cannot change
// what the authorization server is told. Configure it in production.
//
// Pinning the base URL also fixes the redirect URI, which lets every flow share
// one OAuth client: the authorization server metadata is discovered once and
// periodically refreshed, and on a server without client ID metadata document
// support a single dynamic registration serves the whole application instead of
// one per browser, until it expires, ages out, or the discovered authorization
// server changes. Without it nothing is shared, because a registration made for
// one request's origin must never be presented with another's.
func WithPublicURL(base string) Option {
	return func(p *OAuthRouteModule) { p.publicURL = strings.TrimSuffix(base, "/") }
}

// OAuthRoutesFor builds a module that mounts the OAuth client routes for a
// previously RegisterClient'd name, using cfg (client id, scope, optional
// secret) for the authorization request.
func OAuthRoutesFor(name string, cfg oauth.Config, opts ...Option) *OAuthRouteModule {
	p := &OAuthRouteModule{
		name:     name,
		cfg:      cfg,
		store:    defaultStore,
		success:  "/",
		basePath: DefaultBasePath,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Init is a no-op; the module only contributes routes.
func (p *OAuthRouteModule) Init(s *velapp.Services) error { return nil }

// Start is a no-op.
func (p *OAuthRouteModule) Start(s *velapp.Services) error { return nil }

// Shutdown is a no-op.
func (p *OAuthRouteModule) Shutdown(ctx context.Context) error { return nil }

// Routes mounts the redirect and callback handlers on the session-backed web
// stack (so the default SessionStore can persist state). Both are GET, so the
// web stack's CSRF guard does not reject them.
//
// The client ID metadata document is mounted outside that stack: the
// authorization server fetches it while resolving the client_id, with no
// browser, session or credentials involved, so no session, CSRF or
// authentication middleware may stand in front of it.
//
// Every route is named "mcp.oauth.{client}.{route}", so a consumer resolves the
// callback or the document URL through the router instead of rebuilding paths.
func (p *OAuthRouteModule) Routes(r *chain.Routing) {
	r.Web(func(web router.Router) {
		web.Get(p.redirectPath(), p.handleRedirect).Name(p.routeName("redirect"))
		web.Get(p.callbackPath(), p.handleCallback).Name(p.routeName("callback"))
	})
	r.Router().Get(p.metadataPath(), p.handleClientMetadata).Name(p.routeName("client-metadata"))
}

// routeName is the router name one of this client's routes is registered under.
func (p *OAuthRouteModule) routeName(route string) string {
	return "mcp.oauth." + p.name + "." + route
}

// handleRedirect begins the flow: the server's challenge, discovery, PKCE,
// persist pending, then send the browser to the authorization server's consent
// screen.
func (p *OAuthRouteModule) handleRedirect(c *router.Context) error {
	e, ok := lookup(p.name)
	if !ok {
		return c.String(http.StatusInternalServerError, "mcp client ["+p.name+"] is not registered")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), flowTimeout)
	defer cancel()

	origin := p.origin(c)
	cfg := p.cfg
	cfg.RedirectURI = origin + p.callbackPath()
	cfg.ClientIDMetadataURL = origin + p.metadataPath()

	resourceMetadataURL, scope := challengeFrom(ctx, e.resourceURL)
	oc, err := p.oauthClient(e.resourceURL, cfg, resourceMetadataURL, scope)
	if err != nil {
		return c.String(http.StatusInternalServerError, "oauth client: "+err.Error())
	}
	authURL, pending, err := oc.AuthorizationURL(ctx, c.Query("return"))
	if err != nil {
		return c.String(http.StatusBadGateway, "authorization url: "+err.Error())
	}
	if err := p.store.SavePending(c, pending); err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	// authURL targets the authorization server (a different origin) and was
	// produced from validated discovery metadata, not user input, so it is
	// emitted directly: c.Redirect's same-origin guard would rewrite a
	// cross-origin URL to "/".
	c.SetHeader("Location", authURL)
	return c.Status(http.StatusFound)
}

// handleCallback completes the flow: match the pending authorization by state,
// exchange the code for a token, persist it, and redirect onward.
func (p *OAuthRouteModule) handleCallback(c *router.Context) error {
	e, ok := lookup(p.name)
	if !ok {
		return c.String(http.StatusInternalServerError, "mcp client ["+p.name+"] is not registered")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), flowTimeout)
	defer cancel()

	pending, err := p.store.TakePending(c, c.Query("state"))
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}
	if pending == nil {
		return c.String(http.StatusBadRequest, "no pending authorization matches this state")
	}

	oc, err := client.Web(e.resourceURL).WithOAuth(p.cfg).OAuthClient("", "")
	if err != nil {
		return c.String(http.StatusInternalServerError, "oauth client: "+err.Error())
	}
	token, returnTo, err := oc.ExchangeCode(ctx, pending, c.Request.URL.Query())
	if err != nil {
		return c.String(http.StatusBadGateway, "code exchange: "+err.Error())
	}
	if err := p.store.SaveToken(c, p.name, token.AccessToken); err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	dest := returnTo
	if dest == "" {
		dest = p.success
	}
	return c.Redirect(router.StatusFound, dest)
}

// challengeFrom asks the MCP server, without credentials, what it requires of a
// caller, and returns the protected-resource metadata URL and the scope named by
// the challenge it answers with (RFC 9728 5.1). Both are empty when the server
// answers anything else.
//
// The MCP authorization specification has a client use the metadata URL of the
// challenge when there is one and derive the well-known locations only when
// there is not, and a server may publish its metadata through the challenge
// alone. This flow starts from a link in the application, not from a request
// the server refused, so the refusal is asked for here. The URL it names is
// advertised by the server like every other URL of the flow, and is fetched
// under the same guard. It is never taken from the browser: a metadata URL read
// from the query string would let whoever crafted the link choose where this
// application connects to.
func challengeFrom(ctx context.Context, resourceURL string) (resourceMetadataURL, scope string) {
	w := client.Web(resourceURL)
	w.WithTimeout(flowTimeout)
	defer w.Disconnect()

	var required *oauth.AuthorizationRequiredError
	if err := w.Connect(ctx); errors.As(err, &required) {
		return required.ResourceMetadataURL(), required.Scope()
	}
	return "", ""
}

// oauthClient returns the OAuth client that drives one authorization start.
//
// When WithPublicURL pinned this application's base URL, every flow addresses
// the same redirect URI, so one client is shared: it discovers the
// authorization server once and, when the server has no client ID metadata
// document support, reuses the single client record it registered instead of
// creating one per browser. It still prefers the metadata document on every
// flow, so a registration never outranks it.
//
// Without a pinned base URL the redirect URI is derived from the request's Host
// header, which this application does not control, so nothing is shared: a
// registration is bound to the redirect_uri it was created for, and presenting
// it with a different origin's redirect_uri is rejected by the authorization
// server. One request carrying an unexpected Host must not be able to decide
// the identity every later flow uses.
func (p *OAuthRouteModule) oauthClient(resourceURL string, cfg oauth.Config, resourceMetadataURL, scope string) (*oauth.Client, error) {
	if p.publicURL == "" {
		return client.Web(resourceURL).WithOAuth(cfg).OAuthClient(resourceMetadataURL, scope)
	}

	// Built under the lock so that concurrent first flows share one client, and
	// therefore one dynamic registration, rather than racing to create several.
	// The key covers everything the shared client was built from and memoized:
	// the server it discovered, the redirect URI its registration was issued
	// for (which also fixes the document URL, since both are built from the
	// same origin), and the metadata URL and scope of the server's challenge.
	// Two flows built differently must not share one client, so a server that
	// starts naming another metadata document or scope gets a client that
	// discovers it afresh.
	key := strings.Join([]string{resourceURL, cfg.RedirectURI, resourceMetadataURL, scope}, "\n")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shared != nil && p.sharedFor == key {
		return p.shared, nil
	}
	oc, err := client.Web(resourceURL).WithOAuth(cfg).OAuthClient(resourceMetadataURL, scope)
	if err != nil {
		return nil, err
	}
	p.shared, p.sharedFor = oc, key
	return oc, nil
}

// baseURL reconstructs this app's origin from the request.
func baseURL(c *router.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.Header("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

var (
	_ velapp.Module     = (*OAuthRouteModule)(nil)
	_ chain.RouteModule = (*OAuthRouteModule)(nil)
)
