// Package provider wires an MCP server into a Velocity application as a
// first-party chain service provider: typed registration in the component
// registry plus automatic mounting of the streamable-HTTP transport.
//
// Usage:
//
//	srv := server.New("my-app", "1.0.0", server.WithTools(...))
//
//	v, _ := velocity.New()
//	v.Providers(func(r *chain.ProviderRegistry) {
//	    r.Add(provider.New(srv))
//	})
//
// The provider registers the server in the typed component registry (so
// handlers can retrieve it via router.Service[*server.Server](c) or
// server.FromServices), mounts the HTTP transport at /mcp (override with
// WithPath), and the framework's post-bootstrap wiring sweep injects the
// event dispatcher so MCP events (session.initialized, tool.called, ...)
// flow through the app's event system automatically.
package provider

import (
	"context"
	"errors"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/transport"
)

// DefaultPath is the route the MCP HTTP transport mounts at unless overridden
// with WithPath.
const DefaultPath = "/mcp"

// Provider integrates an MCP server into a Velocity application. It
// implements app.ServiceProvider for lifecycle and chain.RouteProvider so the
// HTTP transport route installs automatically when added as a chain provider.
type Provider struct {
	srv         *server.Server
	path        string
	middleware  []router.MiddlewareFunc
	handlerOpts []transport.HandlerOption
}

// Option customises the provider.
type Option func(*Provider)

// WithPath overrides the route the HTTP transport mounts at (default
// DefaultPath). An empty path is ignored and the default is kept.
func WithPath(path string) Option {
	return func(p *Provider) {
		if path != "" {
			p.path = path
		}
	}
}

// WithMiddleware attaches route middleware to the MCP endpoint (auth guards,
// rate limiting, CORS, ...). The transport itself performs no authentication;
// per the transport contract the surrounding middleware owns authorization.
func WithMiddleware(mw ...router.MiddlewareFunc) Option {
	return func(p *Provider) {
		p.middleware = append(p.middleware, mw...)
	}
}

// WithHandlerOptions forwards options to transport.Handler (e.g.
// transport.WithMaxBodyBytes).
func WithHandlerOptions(opts ...transport.HandlerOption) Option {
	return func(p *Provider) {
		p.handlerOpts = append(p.handlerOpts, opts...)
	}
}

// New builds a provider serving srv.
func New(srv *server.Server, opts ...Option) *Provider {
	p := &Provider{srv: srv, path: DefaultPath}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Register stores the server in the typed component registry (key:
// *server.Server). The framework's wiring sweep runs after the provider
// lifecycle and injects the event dispatcher into the server
// (contract.EventDispatcherAware), so MCP events reach app listeners with no
// extra wiring here.
func (p *Provider) Register(s *velapp.Services) error {
	if p.srv == nil {
		return errors.New("mcp: provider constructed without a server")
	}
	return server.RegisterServices(s, p.srv)
}

// Boot is a no-op: the server has no cross-provider dependencies to resolve.
func (p *Provider) Boot(s *velapp.Services) error { return nil }

// Shutdown is a no-op: the server holds no connections or goroutines of its
// own, and per the registry ownership rule any teardown would belong to the
// registry sweep, not the provider.
func (p *Provider) Shutdown(ctx context.Context) error { return nil }

// Routes implements chain.RouteProvider: it mounts the streamable-HTTP
// transport at the configured path on the raw router. The route is
// deliberately NOT placed in the web middleware group: MCP clients are
// programs, not browsers, and the web stack's CSRF/session middleware would
// reject every request. Attach auth via WithMiddleware instead.
func (p *Provider) Routes(r *chain.Routing) {
	route := r.Router().Post(p.path, transport.Handler(p.srv, p.handlerOpts...))
	if len(p.middleware) > 0 {
		route.Use(p.middleware...)
	}
}

var _ velapp.ServiceProvider = (*Provider)(nil)
var _ chain.RouteProvider = (*Provider)(nil)
