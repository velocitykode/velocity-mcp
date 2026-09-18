package oauth

import (
	"net/http"
	"strings"

	"github.com/velocitykode/velocity/router"
	"github.com/velocitykode/velocity/str"
)

// DefaultScope is the scope advertised for MCP access when a Config does not
// name one. A client that receives it in a challenge or in protected-resource
// metadata requests it during authorization.
const DefaultScope = "mcp:use"

// DefaultPrefix is the path prefix the dynamic client registration endpoint is
// mounted under when a Config does not name one, giving POST /oauth/register.
const DefaultPrefix = "oauth"

// wellKnownProtectedResource is the RFC 9728 well-known path for
// protected-resource metadata. The resource path is appended to it, so the
// document for the resource at /mcp lives at
// /.well-known/oauth-protected-resource/mcp.
const wellKnownProtectedResource = "/.well-known/oauth-protected-resource"

// wellKnownAuthorizationServer is the RFC 8414 well-known path for
// authorization-server metadata.
const wellKnownAuthorizationServer = "/.well-known/oauth-authorization-server"

// Config describes this application as an OAuth protected resource, and
// optionally as the authorization server in front of it. The zero value is
// usable: the scope and prefix fall back to their defaults and every URL is
// derived from the inbound request.
type Config struct {
	// BaseURL is the externally visible origin of this application
	// ("https://mcp.example.com", no trailing slash). It is the origin every
	// advertised URL is built from.
	//
	// When empty the origin is derived from each inbound request (its TLS state
	// and Host header). That is convenient in development and correct for a
	// directly exposed server, but behind a TLS-terminating proxy it yields
	// http:// URLs, and a client that reaches the server with an attacker-chosen
	// Host header is handed metadata pointing at that host. Set it explicitly in
	// production: it is the only way the advertised issuer and resource
	// identifiers are pinned to what the operator intends.
	BaseURL string

	// AuthorizationServer is the issuer identifier clients should authorize
	// against (RFC 8414 issuer). Empty means this application is its own
	// authorization server and BaseURL (or the request origin) is the issuer.
	AuthorizationServer string

	// Scope is the OAuth scope required for MCP access, advertised in the
	// challenge and in both metadata documents. Empty means DefaultScope.
	Scope string

	// Prefix is the path prefix the registration endpoint is mounted under.
	// Empty means DefaultPrefix.
	Prefix string

	// AuthorizationEndpoint and TokenEndpoint are this application's OAuth
	// endpoints, either absolute URLs or paths relative to the origin
	// ("/oauth/authorize"). Both are required by RFC 8414, so
	// authorization-server metadata is published only when both are set;
	// a resource server fronting somebody else's authorization server leaves
	// them empty and points clients at AuthorizationServer instead.
	AuthorizationEndpoint string
	TokenEndpoint         string

	// AuthorizationResponseIssuer declares that the authorization endpoint puts
	// the iss parameter on every authorization response it sends, error
	// responses included (RFC 9207 2). It is advertised as
	// authorization_response_iss_parameter_supported, which RFC 9207 2.3
	// requires of a server that sends the parameter, and it is what makes a
	// client refuse a response that lacks it: without the advertisement a
	// client has no way to tell a response whose iss was stripped from one that
	// never had any, and the protection against mix-up attacks is lost. Set it
	// only when the endpoint really does send iss, since a client that was
	// promised the parameter rejects every response without it.
	AuthorizationResponseIssuer bool

	// ClientIDMetadataDocuments declares that the authorization endpoint accepts
	// the HTTPS URL of a client ID metadata document as a client_id, fetching
	// the document to learn about the client. It is advertised as
	// client_id_metadata_document_supported, which is what lets an MCP client
	// skip dynamic registration, the order of preference the MCP authorization
	// specification sets.
	ClientIDMetadataDocuments bool

	// TokenEndpointAuthMethods lists the client authentication methods the
	// token endpoint accepts ("none", "client_secret_post",
	// "client_secret_basic"), advertised as
	// token_endpoint_auth_methods_supported. RFC 8414 2 reads an absent list as
	// client_secret_basic alone, so an empty value here is advertised as "none"
	// whenever dynamic registration is mounted, because the clients it
	// registers are public and authenticate with PKCE alone, and as nothing
	// otherwise.
	TokenEndpointAuthMethods []string

	// Clients backs dynamic client registration (RFC 7591). A nil store leaves
	// the registration endpoint unmounted and registration_endpoint out of the
	// authorization-server metadata, which is the correct advertisement for a
	// deployment that provisions clients out of band.
	Clients ClientStore

	// RedirectDomains restricts the http(s) redirect URIs a dynamically
	// registered client may claim, each entry an origin prefix
	// ("https://example.com"). A single "*" entry allows any domain. An empty
	// list allows none, so an application that mounts registration without
	// deciding this rejects every http(s) redirect rather than accepting every
	// one.
	//
	// An entry is honoured as it is written. One that carries a path
	// ("https://example.com/clients") admits the redirect URIs under that path
	// and no others, and a redirect URI with "." or ".." segments is refused
	// outright, since it would only reach its target from somewhere else. A
	// loopback host written with the http scheme or with none ("http://localhost")
	// admits the RFC 8252 loopback redirect on any port and on any of the
	// loopback hosts; written with another scheme ("https://localhost") it is an
	// origin prefix like any other.
	//
	// It authorizes domains, never transports: a plain-HTTP callback is refused
	// whatever this list says unless its host is a loopback address, because
	// the authorization response would otherwise travel over a connection
	// anyone on the path can read.
	RedirectDomains []string

	// CustomSchemes lists the private-use URI schemes (RFC 8252) native clients
	// may use for their redirect callbacks, without the "://" ("myapp"). A
	// redirect URI whose scheme is neither http, https, nor listed here is
	// rejected.
	CustomSchemes []string

	// ResourceMetadataURL pins the protected-resource metadata URL advertised in
	// the challenge, used exactly as given. It is for a deployment whose
	// metadata document is published somewhere this application cannot derive:
	// another origin, or a single unsuffixed document shared by every MCP route.
	// Empty means the URL is derived per request from the origin and the path
	// being served, which is the RFC 9728 path-insertion rule.
	//
	// It only changes what the challenge advertises. Routes still publishes this
	// application's own documents unless WithoutResourceMetadata is set, which
	// is what a deployment pointing at a foreign document also wants.
	ResourceMetadataURL string

	// WithoutResourceMetadata declares that this application publishes no
	// protected-resource metadata: Routes mounts no protected-resource document,
	// and with no ResourceMetadataURL to advertise instead the challenge names
	// the realm and the scope alone.
	WithoutResourceMetadata bool
}

// scope returns the configured scope, or DefaultScope.
func (cfg Config) scope() string {
	if cfg.Scope == "" {
		return DefaultScope
	}
	return cfg.Scope
}

// scopeTokens splits the configured scope into the individual scope tokens the
// metadata documents advertise.
//
// Scope is one space-delimited value (RFC 6749 3.3), which is the form the
// challenge and a registration request carry. scopes_supported is a JSON array
// of scopes (RFC 8414 2, RFC 9728 2), one element per scope, so a client that
// checks whether the resource understands a single permission finds it. Passing
// the whole value through as one element would hide every scope but the first
// behind a string no client ever compares equal to.
//
// A field that is not a valid scope-token (RFC 6749 3.3 allows visible ASCII
// other than the double quote and the backslash) is dropped rather than
// advertised: a client cannot request it, and it has no place in a JSON
// document describing what may be requested. Repeats collapse, since the
// document describes a set.
func (cfg Config) scopeTokens() []string {
	fields := strings.Fields(cfg.scope())
	tokens := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if !isScopeToken(field) {
			continue
		}
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		tokens = append(tokens, field)
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// isScopeToken reports whether v is a scope-token as RFC 6749 3.3 defines it:
// one or more characters from %x21 / %x23-5B / %x5D-7E, which is printable
// ASCII less the space, the double quote and the backslash.
func isScopeToken(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if ch <= 0x20 || ch >= 0x7f || ch == '"' || ch == '\\' {
			return false
		}
	}
	return true
}

// prefix returns the configured registration prefix with any surrounding
// slashes removed, or DefaultPrefix.
func (cfg Config) prefix() string {
	p := strings.Trim(cfg.Prefix, "/")
	if p == "" {
		return DefaultPrefix
	}
	return p
}

// registerPath returns the absolute router path of the registration endpoint.
func (cfg Config) registerPath() string {
	return "/" + cfg.prefix() + "/register"
}

// origin returns the externally visible origin for a request: the configured
// BaseURL when set, otherwise the request's own scheme and host. Forwarding
// headers are deliberately not consulted; they are client-supplied and would
// let a caller choose the origin this server advertises.
func (cfg Config) origin(r *http.Request) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// absolute resolves an endpoint that may be given as an absolute URL or as a
// path relative to origin. An empty endpoint yields an empty string.
func absolute(origin, endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	return origin + "/" + strings.TrimLeft(endpoint, "/")
}

// resourcePath normalizes the path of a protected resource into the suffix that
// is appended to the well-known metadata path: a leading slash on anything
// non-empty, and nothing else touched.
//
// A trailing slash is part of the identifier and is kept. RFC 9728 3.1 builds
// the metadata URL by inserting the well-known path between the host and the
// path of the resource identifier, and 3.3 has the client require the resource
// in the document to be that same identifier, so https://host/mcp and
// https://host/mcp/ are two resources: answering for one with the other leaves
// the client with a document it must reject and no way to start authorization.
//
// The one path not kept is a bare "/". A request line always carries at least a
// slash, so it cannot tell the identifier https://host from https://host/, and
// RFC 3986 6.2.3 makes those the same URI in any case. It yields "", which
// addresses the unsuffixed metadata document.
func resourcePath(path string) string {
	if path == "" || path == "/" {
		return ""
	}
	return str.Start(path, "/")
}

// ProtectedResourceMetadataURL returns the URL of the protected-resource
// metadata document describing the resource served at path under origin,
// following the RFC 9728 path-insertion rule: the resource path is appended to
// the well-known path, so /mcp/weather is described by
// /.well-known/oauth-protected-resource/mcp/weather.
//
// path is a decoded request path. Each of its segments is percent-encoded on
// the way in, the same encoding the resource identifier in the document itself
// carries, so the URL stays syntactically valid for any path the router matched
// and the two never disagree about the same resource.
func ProtectedResourceMetadataURL(origin, path string) string {
	return metadataURLFor(origin, escapePath(resourcePath(path)))
}

// metadataURLFor builds the protected-resource metadata URL from a resource
// path that is already encoded, the form both the challenge and the documents
// work in.
func metadataURLFor(origin, encodedPath string) string {
	return origin + wellKnownProtectedResource + encodedPath
}

// requestPath returns the path of the resource a router context is serving,
// encoded exactly as the client spelled it. The encoding is taken from the
// request rather than rebuilt from the decoded path so the identifier the
// client used is what comes back to it.
func requestPath(c *router.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	return resourcePath(c.Request.URL.EscapedPath())
}

// metadataResourcePath returns the path of the resource a metadata request is
// asking about: whatever the request path carries after the well-known prefix,
// exactly as it was spelled. That reverses the RFC 9728 3.1 insertion, so the
// identifier in the document is the one the client built the request URL from,
// which is what RFC 9728 3.3 has it compare against.
//
// The route capture cannot be used for this: the router collapses and trims
// slashes before matching, so a trailing slash never reaches it. It is the
// fallback for a router serving these documents under some other prefix, where
// the insertion cannot be reversed.
func metadataResourcePath(c *router.Context, param string) string {
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path := c.Request.URL.EscapedPath()
		if str.Contains(path, wellKnownProtectedResource) {
			return str.After(path, wellKnownProtectedResource)
		}
	}
	return escapePath(resourcePath(param))
}
