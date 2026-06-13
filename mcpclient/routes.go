package mcpclient

import (
	"context"
	"net/http"
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

// OAuthRouteProvider mounts the authorization-code routes for one registered
// MCP server. It implements chain.RouteProvider (so it installs when added to
// the provider registry) and app.ServiceProvider (so it is addable via
// reg.Add).
type OAuthRouteProvider struct {
	name     string
	cfg      oauth.Config
	store    Store
	success  string
	basePath string
}

// Option customises an OAuthRouteProvider.
type Option func(*OAuthRouteProvider)

// WithStore overrides the token/pending store (default: the package default
// store, SessionStore).
func WithStore(s Store) Option { return func(p *OAuthRouteProvider) { p.store = s } }

// WithSuccessRedirect sets the path to redirect to after a successful exchange
// when the flow carried no return target (default "/").
func WithSuccessRedirect(path string) Option {
	return func(p *OAuthRouteProvider) { p.success = path }
}

// WithBasePath overrides the route prefix (default DefaultBasePath).
func WithBasePath(prefix string) Option {
	return func(p *OAuthRouteProvider) {
		if prefix != "" {
			p.basePath = prefix
		}
	}
}

// OAuthRoutesFor builds a provider that mounts the OAuth client routes for a
// previously RegisterClient'd name, using cfg (client id, scope, optional
// secret) for the authorization request.
func OAuthRoutesFor(name string, cfg oauth.Config, opts ...Option) *OAuthRouteProvider {
	p := &OAuthRouteProvider{
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

// Register is a no-op; the provider only contributes routes.
func (p *OAuthRouteProvider) Register(s *velapp.Services) error { return nil }

// Boot is a no-op.
func (p *OAuthRouteProvider) Boot(s *velapp.Services) error { return nil }

// Shutdown is a no-op.
func (p *OAuthRouteProvider) Shutdown(ctx context.Context) error { return nil }

// Routes mounts the redirect and callback handlers on the session-backed web
// stack (so the default SessionStore can persist state). Both are GET, so the
// web stack's CSRF guard does not reject them.
func (p *OAuthRouteProvider) Routes(r *chain.Routing) {
	base := p.basePath + "/" + p.name
	r.Web(func(web router.Router) {
		web.Get(base+"/redirect", p.handleRedirect)
		web.Get(base+"/callback", p.handleCallback)
	})
}

// handleRedirect begins the flow: discovery, PKCE, persist pending, then send
// the browser to the authorization server's consent screen.
func (p *OAuthRouteProvider) handleRedirect(c *router.Context) error {
	e, ok := lookup(p.name)
	if !ok {
		return c.String(http.StatusInternalServerError, "mcp client ["+p.name+"] is not registered")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), flowTimeout)
	defer cancel()

	cfg := p.cfg
	cfg.RedirectURI = baseURL(c) + p.basePath + "/" + p.name + "/callback"

	oc, err := client.Web(e.resourceURL).WithOAuth(cfg).OAuthClient("", "")
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
func (p *OAuthRouteProvider) handleCallback(c *router.Context) error {
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

// baseURL reconstructs this app's origin from the request.
func baseURL(c *router.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.Header("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

var (
	_ velapp.ServiceProvider = (*OAuthRouteProvider)(nil)
	_ chain.RouteProvider    = (*OAuthRouteProvider)(nil)
)
