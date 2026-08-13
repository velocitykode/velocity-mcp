// Package module wires an MCP server into a Velocity application as a
// first-party chain module: typed registration in the component
// registry plus automatic mounting of the streamable-HTTP transport.
//
// Usage:
//
//	srv := server.New("my-app", "1.0.0", server.WithTools(...))
//
//	v, _ := velocity.New()
//	v.Modules(func(r *chain.ModuleRegistry) {
//	    r.Add(module.New(srv))
//	})
//
// The module registers the server in the typed component registry (so
// handlers can retrieve it via router.Service[*server.Server](c) or
// server.FromServices), mounts the HTTP transport at /mcp (override with
// WithPath), and the framework's post-bootstrap wiring sweep injects the
// event dispatcher so MCP events (session.initialized, tool.called, ...)
// flow through the app's event system automatically.
package module

import (
	"context"
	"errors"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/console"
	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/transport"
)

// DefaultPath is the route the MCP HTTP transport mounts at unless overridden
// with WithPath.
const DefaultPath = "/mcp"

// Module integrates an MCP server into a Velocity application. It
// implements app.Module for lifecycle and chain.RouteModule so the
// HTTP transport route installs automatically when added as a chain module.
type Module struct {
	srv         *server.Server
	path        string
	middleware  []router.MiddlewareFunc
	handlerOpts []transport.HandlerOption
}

// Option customises the module.
type Option func(*Module)

// WithPath overrides the route the HTTP transport mounts at (default
// DefaultPath). An empty path is ignored and the default is kept.
func WithPath(path string) Option {
	return func(m *Module) {
		if path != "" {
			m.path = path
		}
	}
}

// WithMiddleware attaches route middleware to the MCP endpoint (auth guards,
// rate limiting, CORS, ...). The transport itself performs no authentication;
// per the transport contract the surrounding middleware owns authorization.
func WithMiddleware(mw ...router.MiddlewareFunc) Option {
	return func(m *Module) {
		m.middleware = append(m.middleware, mw...)
	}
}

// WithHandlerOptions forwards options to transport.Handler (e.g.
// transport.WithMaxBodyBytes).
func WithHandlerOptions(opts ...transport.HandlerOption) Option {
	return func(m *Module) {
		m.handlerOpts = append(m.handlerOpts, opts...)
	}
}

// New builds a module serving srv.
func New(srv *server.Server, opts ...Option) *Module {
	m := &Module{srv: srv, path: DefaultPath}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Init stores the server in the typed component registry (key:
// *server.Server). The framework's wiring sweep runs after module
// lifecycle and injects the event dispatcher into the server
// (contract.EventDispatcherAware), so MCP events reach app listeners with no
// extra wiring here.
func (m *Module) Init(s *velapp.Services) error {
	if m.srv == nil {
		return errors.New("mcp: module constructed without a server")
	}
	return server.RegisterServices(s, m.srv)
}

// Start is a no-op: the server has no cross-module dependencies to resolve.
func (m *Module) Start(s *velapp.Services) error { return nil }

// Shutdown is a no-op: the server holds no connections or goroutines of its
// own, and per the registry ownership rule any teardown would belong to the
// registry sweep, not the module.
func (m *Module) Shutdown(ctx context.Context) error { return nil }

// Routes implements chain.RouteModule: it mounts the streamable-HTTP
// transport at the configured path on the raw router. The route is
// deliberately NOT placed in the web middleware group: MCP clients are
// programs, not browsers, and the web stack's CSRF/session middleware would
// reject every request. Attach auth via WithMiddleware instead.
func (m *Module) Routes(r *chain.Routing) {
	route := r.Router().Post(m.path, transport.Handler(m.srv, m.handlerOpts...))
	if len(m.middleware) > 0 {
		route.Use(m.middleware...)
	}
}

// Commands implements chain.CommandModule: it registers the MCP code
// generators (make:mcp-tool, make:mcp-resource, make:mcp-prompt) plus the
// runtime commands bound to the served server (mcp:start to serve over stdio,
// mcp:inspect to list registered primitives), so an app that adds this module
// gets all of them under `vel run ...`. The generators are server-independent;
// the runtime commands operate on m.srv.
func (m *Module) Commands(r *chain.Commands) {
	r.Add(console.Generators()...)
	r.Add(console.ServerCommands(m.srv)...)
}

var _ velapp.Module = (*Module)(nil)
var _ chain.RouteModule = (*Module)(nil)
var _ chain.CommandModule = (*Module)(nil)
