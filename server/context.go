package server

import (
	"context"
	"sync"

	"github.com/velocitykode/velocity-mcp/schema"
)

// Context is the per-request server context (laravel/mcp's ServerContext). It
// exposes the server's negotiated protocol version, advertised capabilities,
// implementation metadata, instructions, registered primitives, pagination
// bounds, and a mutex-protected per-session state store the method handlers and
// primitive handlers can read and write.
//
// A Context is created by the Server for each handled message via NewContext.
// Its primitive accessors return the registered tools/resources/prompts so the
// method handlers do not reach back into the Server.
type Context struct {
	implementation schema.Implementation
	instructions   string

	supportedVersions []ProtocolVersion
	capabilities      map[string]any

	tools     []Tool
	resources []Resource
	templates []URITemplate
	prompts   []Prompt

	maxPageSize     int
	defaultPageSize int

	// sessionID is the transport session id for the message being handled, or
	// "" before a session is established (e.g. the initialize request itself).
	sessionID string

	// ctx is the inbound request context threaded from the transport (the HTTP
	// request context or the stdio loop context). It is passed to primitive
	// handlers so a long-running tool/resource/prompt can observe client
	// cancellation and request deadlines. Never nil after NewContext.
	ctx context.Context

	// negotiated is the protocol version agreed during initialize; empty until
	// negotiated. Guarded by mu.
	negotiated string

	// state is the per-session scratch store. Guarded by mu so concurrent
	// primitive handlers sharing a session do not race on the map.
	state map[string]any
	mu    sync.RWMutex
}

// NewContext builds a server context from a server's resolved configuration. It
// is called by Server.createContext; tests may call it directly.
func NewContext(impl schema.Implementation, instructions string, versions []ProtocolVersion, capabilities map[string]any) *Context {
	return &Context{
		implementation:    impl,
		instructions:      instructions,
		supportedVersions: append([]ProtocolVersion(nil), versions...),
		capabilities:      cloneCapabilities(capabilities),
		maxPageSize:       defaultMaxPageSize,
		defaultPageSize:   defaultPageSize,
		state:             make(map[string]any),
		ctx:               context.Background(),
	}
}

// RequestContext returns the inbound request context threaded from the
// transport. Method and primitive handlers pass it to tool/resource/prompt
// invocations so a long-running handler observes client cancellation and the
// request deadline. It is never nil; an unset context defaults to
// context.Background().
func (c *Context) RequestContext() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// withRequestContext records the inbound request context on the Context. It is
// called by Server.createContext; an empty ctx defaults to context.Background().
func (c *Context) withRequestContext(ctx context.Context) *Context {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ctx = ctx
	return c
}

// Implementation returns the server implementation metadata advertised during
// initialize.
func (c *Context) Implementation() schema.Implementation { return c.implementation }

// Instructions returns the server's instructions string.
func (c *Context) Instructions() string { return c.instructions }

// SessionID returns the transport session id for the current message, or "".
func (c *Context) SessionID() string { return c.sessionID }

// SupportedProtocolVersions returns the ordered list of protocol versions the
// server supports, newest first. The first element is the negotiation fallback.
func (c *Context) SupportedProtocolVersions() []ProtocolVersion {
	return append([]ProtocolVersion(nil), c.supportedVersions...)
}

// Capabilities returns a copy of the advertised server capabilities map.
func (c *Context) Capabilities() map[string]any { return cloneCapabilities(c.capabilities) }

// HasCapability reports whether the named capability is advertised, mirroring
// laravel/mcp's ServerContext::hasCapability.
func (c *Context) HasCapability(name string) bool {
	_, ok := c.capabilities[name]
	return ok
}

// Tools returns the registered tools.
func (c *Context) Tools() []Tool { return c.tools }

// Resources returns the registered non-template resources.
func (c *Context) Resources() []Resource { return c.resources }

// ResourceTemplates returns the registered URI-template resources.
func (c *Context) ResourceTemplates() []URITemplate { return c.templates }

// Prompts returns the registered prompts.
func (c *Context) Prompts() []Prompt { return c.prompts }

// PerPage resolves the page size for a list request: the smaller of the
// requested size and the server max, falling back to the default only when no
// size is requested (requested is nil). It mirrors laravel/mcp's
// ServerContext::perPage, which computes min($requestedPerPage ?? $default,
// $max): the ?? default fires only on a null (absent) value, so an explicit
// per_page of 0 (or negative) yields min(0, max) = 0 (an empty page), NOT the
// default. Passing a nil pointer means the client omitted per_page.
func (c *Context) PerPage(requested *int) int {
	size := c.defaultPageSize
	if requested != nil {
		size = *requested
	}
	if size > c.maxPageSize {
		size = c.maxPageSize
	}
	return size
}

// NegotiatedVersion returns the protocol version agreed during initialize, or
// "" before negotiation.
func (c *Context) NegotiatedVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.negotiated
}

// SetNegotiatedVersion records the protocol version agreed during initialize.
func (c *Context) SetNegotiatedVersion(version string) {
	c.mu.Lock()
	c.negotiated = version
	c.mu.Unlock()
}

// State returns the value stored under key in the per-session state and whether
// it was present. Safe for concurrent use.
func (c *Context) State(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.state[key]
	return v, ok
}

// SetState stores value under key in the per-session state. Safe for concurrent
// use.
func (c *Context) SetState(key string, value any) {
	c.mu.Lock()
	if c.state == nil {
		c.state = make(map[string]any)
	}
	c.state[key] = value
	c.mu.Unlock()
}

// cloneCapabilities returns an independent shallow copy of a capabilities map so
// the Context cannot be mutated through a shared reference.
func cloneCapabilities(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
