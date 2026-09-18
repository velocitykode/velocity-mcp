package mcpclient

import (
	"net/http"
	"strings"

	"github.com/velocitykode/velocity/collect"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// ClientMetadataFile is the file name the client ID metadata document is served
// under, appended to a client's route prefix.
const ClientMetadataFile = "/client-metadata.json"

// Cache directives for the client ID metadata document. A document built from
// a pinned public URL is identical for every fetch and carries no per-browser
// state, so authorization servers and any intermediary may cache it. A document
// assembled from the request instead varies with the Host and X-Forwarded-Proto
// headers, neither of which a shared cache keys on, so it must not be stored.
const (
	clientMetadataCacheControl   = "public, max-age=3600"
	clientMetadataNoCacheControl = "no-store"
)

// credentialMetadataKeys are registration-response fields that must never be
// published: they are this application's credentials, not its description.
var credentialMetadataKeys = []string{"client_secret", "client_secret_expires_at", "registration_access_token"}

// handleClientMetadata serves the client ID metadata document. Authorization
// servers fetch it unauthenticated while resolving the client_id, so it is
// mounted outside the web stack and carries no session or user state.
func (p *OAuthRouteModule) handleClientMetadata(c *router.Context) error {
	c.SetHeader("Cache-Control", p.cacheControl())
	return c.JSON(http.StatusOK, p.clientMetadataDocument(p.origin(c)))
}

// cacheControl reports whether this document may be stored by shared caches.
func (p *OAuthRouteModule) cacheControl() string {
	if p.publicURL == "" {
		return clientMetadataNoCacheControl
	}
	return clientMetadataCacheControl
}

// clientMetadataDocument builds the published document for an origin. The
// caller-supplied metadata (WithClientMetadata) contributes descriptive fields
// such as client_name, logo_uri and client_uri, but the fields that decide what
// the authorization server will accept are computed here and cannot be
// overridden: client_id must be the document's own URL, redirect_uris must
// contain the callback this module actually serves, and the application is a
// public client, so token_endpoint_auth_method is always none.
func (p *OAuthRouteModule) clientMetadataDocument(origin string) map[string]any {
	doc := map[string]any{
		"client_name":    oauth.DefaultClientName,
		"client_uri":     origin,
		"grant_types":    []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"},
	}
	for key, value := range p.clientMetadata {
		if isCredentialKey(key) {
			continue
		}
		doc[key] = value
	}
	doc["client_id"] = origin + p.metadataPath()
	doc["redirect_uris"] = p.redirectURIs(origin)
	doc["token_endpoint_auth_method"] = "none"
	return doc
}

// redirectURIs lists the callback this module serves first, followed by any
// additional URIs the consumer declared, with duplicates removed.
func (p *OAuthRouteModule) redirectURIs(origin string) []string {
	uris := []string{origin + p.callbackPath()}
	uris = append(uris, stringsOf(p.clientMetadata["redirect_uris"])...)
	return collect.Unique(uris)
}

// origin returns the absolute base URL the document describes: the configured
// public URL when set, otherwise the origin of the incoming request. Configure
// it in production so the published client_id stays stable regardless of the
// Host header a fetch arrives with.
func (p *OAuthRouteModule) origin(c *router.Context) string {
	if p.publicURL != "" {
		return p.publicURL
	}
	return baseURL(c)
}

// prefix is the route prefix shared by this client's routes.
func (p *OAuthRouteModule) prefix() string { return p.basePath + "/" + p.name }

// redirectPath is the path that begins the authorization flow.
func (p *OAuthRouteModule) redirectPath() string { return p.prefix() + "/redirect" }

// callbackPath is the path the authorization server redirects back to.
func (p *OAuthRouteModule) callbackPath() string { return p.prefix() + "/callback" }

// metadataPath is the path the client ID metadata document is published at.
func (p *OAuthRouteModule) metadataPath() string {
	if p.metadataURI != "" {
		return p.metadataURI
	}
	return p.prefix() + ClientMetadataFile
}

// isCredentialKey reports whether a metadata key carries client credentials.
func isCredentialKey(key string) bool {
	for _, k := range credentialMetadataKeys {
		if key == k {
			return true
		}
	}
	return false
}

// stringsOf coerces a consumer-supplied redirect_uris value ([]string or the
// []any a decoded JSON document yields) to a string slice, dropping anything
// that is not a non-empty string.
func stringsOf(value any) []string {
	switch v := value.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

// normalizePath makes a consumer-supplied route path absolute and free of a
// trailing slash, so it can be appended to an origin.
func normalizePath(path string) string {
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimSuffix(path, "/")
}
