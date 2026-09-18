package oauth

import (
	"net/http"

	"github.com/velocitykode/velocity/router"
)

// Route names for the endpoints this package mounts. They are stable
// identifiers an application can look for when it wants to know whether
// discovery is already published, or override a document with its own handler.
const (
	RouteProtectedResource         = "mcp.oauth.protected-resource"
	RouteProtectedResourceNested   = "mcp.oauth.protected-resource.nested"
	RouteAuthorizationServer       = "mcp.oauth.authorization-server"
	RouteAuthorizationServerNested = "mcp.oauth.authorization-server.nested"
	RouteRegister                  = "mcp.oauth.register"
)

// wildcardParam is the capture name of the resource path suffix in the nested
// well-known routes.
const wildcardParam = "path"

// Routes mounts the OAuth discovery endpoints on r:
//
//	GET  /.well-known/oauth-protected-resource            protected-resource metadata for the root resource
//	GET  /.well-known/oauth-protected-resource/<path>     ... for the resource at <path>
//	GET  /.well-known/oauth-authorization-server          authorization-server metadata
//	GET  /.well-known/oauth-authorization-server/<path>   ... for a nested resource, same document
//	POST /<prefix>/register                               dynamic client registration (application/json only)
//
// All of them are conditional. The protected-resource documents are skipped
// when the Config declares WithoutResourceMetadata, which is the application
// saying it publishes none; the challenge middleware then advertises none
// either, so the two never disagree. The authorization-server documents are
// published only when the Config names both an authorization and a token
// endpoint, because RFC 8414 requires both and a resource server fronting a
// separate issuer has neither to offer. The registration endpoint is mounted
// only when a ClientStore backs it.
//
// The nested routes exist because a client derives the metadata location by
// appending the resource path to the well-known path, so an MCP server mounted
// at /mcp is described at /.well-known/oauth-protected-resource/mcp. Both
// nested routes accept any depth of path.
//
// An exact document an application already serves itself is left alone: a route
// registered under the same method and path, or under one of the names above,
// suppresses this package's version of it. Mounting twice is therefore a no-op
// the second time rather than a duplicate registration.
func Routes(r router.Router, cfg Config) {
	if r == nil {
		return
	}
	taken := registeredRoutes(r)

	if !cfg.WithoutResourceMetadata {
		if !taken.has(http.MethodGet, wellKnownProtectedResource, RouteProtectedResource) {
			r.Get(wellKnownProtectedResource, cfg.protectedResourceHandler("")).
				Name(RouteProtectedResource)
		}

		nestedResource := wellKnownProtectedResource + "/{" + wildcardParam + ":.*}"
		if !taken.has(http.MethodGet, nestedResource, RouteProtectedResourceNested) {
			r.Get(nestedResource, cfg.protectedResourceHandler(wildcardParam)).
				Name(RouteProtectedResourceNested)
		}
	}

	if cfg.servesAuthorizationServer() {
		if !taken.has(http.MethodGet, wellKnownAuthorizationServer, RouteAuthorizationServer) {
			r.Get(wellKnownAuthorizationServer, cfg.authorizationServerHandler()).
				Name(RouteAuthorizationServer)
		}
		nestedServer := wellKnownAuthorizationServer + "/{" + wildcardParam + ":.*}"
		if !taken.has(http.MethodGet, nestedServer, RouteAuthorizationServerNested) {
			r.Get(nestedServer, cfg.authorizationServerHandler()).
				Name(RouteAuthorizationServerNested)
		}
	}

	if cfg.Clients != nil {
		path := cfg.registerPath()
		if !taken.has(http.MethodPost, path, RouteRegister) {
			// A registration request is application/json (RFC 7591 3.1), and the
			// rule is more than tidiness on an endpoint nobody authenticates to:
			// a browser sends text/plain and form bodies across origins without
			// asking first, so accepting them would let any page register
			// clients from its visitors' addresses, under whatever limits the
			// application sets per address.
			r.Post(path, cfg.registerHandler()).
				Name(RouteRegister).
				Use(router.ContentTypeJSON(), router.BodyLimit(MaxRegistrationBodyBytes))
		}
	}
}

// servesAuthorizationServer reports whether this application has enough
// configuration to publish RFC 8414 metadata: both required endpoints.
func (cfg Config) servesAuthorizationServer() bool {
	return cfg.AuthorizationEndpoint != "" && cfg.TokenEndpoint != ""
}

// protectedResourceHandler serves protected-resource metadata. param names the
// route capture holding the resource path suffix, or "" for the route that
// describes the root resource.
func (cfg Config) protectedResourceHandler(param string) router.HandlerFunc {
	return func(c *router.Context) error {
		captured := ""
		if param != "" {
			captured = c.Param(param)
		}
		return c.JSON(http.StatusOK, cfg.protectedResource(cfg.origin(c.Request), metadataResourcePath(c, captured)))
	}
}

// authorizationServerHandler serves authorization-server metadata. The nested
// route serves the same document as the exact one: the issuer is a property of
// the application, not of the resource path the client happened to append.
func (cfg Config) authorizationServerHandler() router.HandlerFunc {
	return func(c *router.Context) error {
		return c.JSON(http.StatusOK, cfg.authorizationServer(cfg.origin(c.Request)))
	}
}

// routeLister is the optional introspection surface velocity's router offers.
// A router that does not implement it (a group, a test double) simply gets no
// override detection.
type routeLister interface {
	AllRoutes() []router.RouteInfo
}

// routeSet indexes the routes already registered on a router by method+path and
// by name, the two ways this package's registrations can collide.
type routeSet struct {
	paths map[string]struct{}
	names map[string]struct{}
}

// has reports whether either the method+path pair or the name is already taken.
func (s routeSet) has(method, path, name string) bool {
	if _, ok := s.paths[method+" "+path]; ok {
		return true
	}
	_, ok := s.names[name]
	return ok
}

// registeredRoutes snapshots what r already serves, or an empty set when r does
// not support introspection.
func registeredRoutes(r router.Router) routeSet {
	set := routeSet{paths: map[string]struct{}{}, names: map[string]struct{}{}}
	lister, ok := r.(routeLister)
	if !ok {
		return set
	}
	for _, info := range lister.AllRoutes() {
		set.paths[info.Method+" "+info.Path] = struct{}{}
		if info.Name != "" {
			set.names[info.Name] = struct{}{}
		}
	}
	return set
}
