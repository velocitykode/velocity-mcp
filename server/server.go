package server

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/velocitykode/velocity/contract"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// Pagination defaults mirror laravel/mcp's Server::$defaultPaginationLength and
// $maxPaginationLength.
const (
	defaultPageSize    = 15
	defaultMaxPageSize = 50
)

// defaultInstructions mirrors the spirit of laravel/mcp's default instructions
// without naming a specific host framework.
const defaultInstructions = "This MCP server lets AI agents interact with the application."

// Method is a single MCP protocol method handler (e.g. tools/call). It mirrors
// laravel/mcp's Server\Contracts\Method. Handlers receive the per-request
// server Context and the parsed JSON-RPC request and return a JSON-RPC response.
//
// A handler reports a protocol-level failure by returning a *jsonrpc.Error
// (wrapped or direct); the Server turns it into an error response. A handler
// returns a nil error and a non-nil response on success. Handlers must never
// panic and must never leak internal error detail into the response.
type Method interface {
	Handle(c *Context, req *jsonrpc.Request) (*jsonrpc.Response, error)
}

// methodFactory builds the default method set. It is installed by the methods
// package via SetMethodFactory in its init, so server never imports methods
// (which would cycle) yet any program that imports methods gets the full
// protocol wired automatically. A Server with no factory installed answers only
// initialize and ping (the two methods server implements directly), and returns
// MethodNotFound for everything else.
var methodFactory atomic.Pointer[func() map[string]Method]

// SetMethodFactory installs the factory used to populate a server's method set.
// It is called once from the methods package init. Calling it again replaces
// the factory (last writer wins); this is not expected outside of that init.
func SetMethodFactory(fn func() map[string]Method) {
	methodFactory.Store(&fn)
}

// SessionIDGenerator produces a unique session id for each initialized session.
// It is overridable (primarily for deterministic tests); the default uses a
// crypto-random source.
type SessionIDGenerator func() string

// Server is an MCP server: a named, versioned implementation exposing tools,
// resources, and prompts over a transport. It mirrors laravel/mcp's Server.
//
// Construct one with New and configure it with the With* options. A Server is
// safe for concurrent use once constructed: its configuration is immutable
// after New returns, and the event dispatcher is stored atomically.
type Server struct {
	name         string
	version      string
	instructions string
	icons        []schema.Icon
	websiteURL   string
	title        string

	capabilities map[string]any
	versions     []ProtocolVersion

	tools     []Tool
	resources []Resource
	templates []URITemplate
	prompts   []Prompt

	maxPageSize     int
	defaultPageSize int

	logger contract.Logger

	// dispatcher holds the framework event dispatcher func. Stored atomically
	// (same pattern as velocity auth.Manager) so request paths read it without
	// locking. Nil holder means events are no-ops.
	dispatcher atomic.Pointer[dispatcherHolder]

	// newSessionID generates session ids. Guarded by mu for test overrides.
	newSessionID SessionIDGenerator
	mu           sync.RWMutex

	// methods is the resolved per-server method set, built once at construction
	// from the installed factory plus the directly-implemented methods.
	methods map[string]Method
}

// dispatcherHolder boxes the dispatcher func so atomic.Pointer can store the
// two-word func value as a single addressable cell.
type dispatcherHolder struct {
	fn func(ctx context.Context, event any) error
}

// Option configures a Server during New.
type Option func(*Server)

// New constructs a Server with the given name and version and applies the
// options. It never panics; an empty name or version is allowed (the client
// simply sees empty implementation metadata).
func New(name, version string, opts ...Option) *Server {
	s := &Server{
		name:            name,
		version:         version,
		instructions:    defaultInstructions,
		capabilities:    defaultCapabilities(),
		versions:        supportedProtocolVersions(),
		maxPageSize:     defaultMaxPageSize,
		defaultPageSize: defaultPageSize,
		newSessionID:    randomSessionID,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	s.methods = s.buildMethods()
	return s
}

// defaultCapabilities returns the capabilities advertised by default, mirroring
// laravel/mcp's Server::$capabilities (tools, resources, prompts each with
// listChanged:false).
func defaultCapabilities() map[string]any {
	return map[string]any{
		CapabilityTools:     map[string]any{"listChanged": false},
		CapabilityResources: map[string]any{"listChanged": false},
		CapabilityPrompts:   map[string]any{"listChanged": false},
	}
}

// buildMethods resolves the server's method set: the installed default factory
// (when the methods package is imported) provides the protocol handlers. The
// server always serves ping and initialize directly (see Handle), so they need
// not appear in the map; they are included from the factory when present for
// uniformity.
func (s *Server) buildMethods() map[string]Method {
	out := map[string]Method{}
	if p := methodFactory.Load(); p != nil && *p != nil {
		for name, m := range (*p)() {
			out[name] = m
		}
	}
	return out
}

// Name returns the server name.
func (s *Server) Name() string { return s.name }

// Version returns the server version.
func (s *Server) Version() string { return s.version }

// Instructions returns the server instructions string.
func (s *Server) Instructions() string { return s.instructions }

// Tools returns the registered tools.
func (s *Server) Tools() []Tool { return s.tools }

// Resources returns the registered non-template resources.
func (s *Server) Resources() []Resource { return s.resources }

// ResourceTemplates returns the registered URI-template resources.
func (s *Server) ResourceTemplates() []URITemplate { return s.templates }

// Prompts returns the registered prompts.
func (s *Server) Prompts() []Prompt { return s.prompts }

// implementation builds the schema.Implementation advertised during initialize.
func (s *Server) implementation() schema.Implementation {
	impl := schema.NewImplementation(s.name, s.version)
	if s.title != "" {
		impl = impl.WithTitle(s.title)
	}
	if s.websiteURL != "" {
		impl = impl.WithWebsiteURL(s.websiteURL)
	}
	if len(s.icons) > 0 {
		impl = impl.WithIcons(s.icons...)
	}
	return impl
}

// createContext builds a fresh per-message server context, mirroring
// laravel/mcp's Server::createContext. sessionID is the transport's session id
// for the message being handled (empty for the initialize request). ctx is the
// inbound request context, threaded onto the Context so primitive handlers can
// observe client cancellation and request deadlines.
func (s *Server) createContext(ctx context.Context, sessionID string) *Context {
	c := NewContext(s.implementation(), s.instructions, s.versions, s.capabilities)
	c.tools = s.tools
	c.resources = s.resources
	c.templates = s.templates
	c.prompts = s.prompts
	c.maxPageSize = s.maxPageSize
	c.defaultPageSize = s.defaultPageSize
	c.sessionID = sessionID
	c.withRequestContext(ctx)
	return c
}

// NewTestContext builds a server Context populated with the server's registered
// primitives and configuration, for unit-testing method handlers without
// driving a full message through Handle. It mirrors velocity's
// router.NewTestContext convenience. sessionID is optional; pass "" for none.
func (s *Server) NewTestContext(sessionID ...string) *Context {
	id := ""
	if len(sessionID) > 0 {
		id = sessionID[0]
	}
	return s.createContext(context.Background(), id)
}

// SetEventDispatcher installs the framework event dispatcher used to emit MCP
// events. It implements contract.EventDispatcherAware so velocity auto-wires
// the server when it is registered in app.Services.Extensions. Passing nil
// detaches the dispatcher (events become no-ops). Safe for concurrent use.
func (s *Server) SetEventDispatcher(fn func(ctx context.Context, event any) error) {
	if fn == nil {
		s.dispatcher.Store(nil)
		return
	}
	s.dispatcher.Store(&dispatcherHolder{fn: fn})
}

// dispatch emits an event through the wired dispatcher. It is a no-op when no
// dispatcher is installed. A dispatcher error is logged (when a logger is set)
// and otherwise swallowed: event delivery must never break request handling.
func (s *Server) dispatch(ctx context.Context, event any) {
	holder := s.dispatcher.Load()
	if holder == nil || holder.fn == nil {
		return
	}
	if err := holder.fn(ctx, event); err != nil && s.logger != nil {
		s.logger.Error("mcp: event dispatch failed", "error", err.Error())
	}
}

// SetSessionIDGenerator overrides the session id generator, primarily for
// deterministic tests. A nil generator restores the default.
func (s *Server) SetSessionIDGenerator(fn SessionIDGenerator) {
	s.mu.Lock()
	if fn == nil {
		s.newSessionID = randomSessionID
	} else {
		s.newSessionID = fn
	}
	s.mu.Unlock()
}

// sessionID generates a new session id using the configured generator.
func (s *Server) sessionID() string {
	s.mu.RLock()
	gen := s.newSessionID
	s.mu.RUnlock()
	return gen()
}
