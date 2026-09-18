package oauth

import (
	"net/url"
	"strings"
)

// responseTypeCode is the only authorization response type MCP clients use.
const responseTypeCode = "code"

// codeChallengeMethodS256 is the PKCE code challenge method required of MCP
// clients; the plain method is deliberately not advertised.
const codeChallengeMethodS256 = "S256"

// Grant types advertised to clients: the authorization code grant MCP clients
// run, plus the refresh token grant they use to stay connected.
const (
	grantAuthorizationCode = "authorization_code"
	grantRefreshToken      = "refresh_token"
)

// ProtectedResourceMetadata describes this MCP endpoint as an OAuth protected
// resource (RFC 9728). A client fetches it from the URL named in the
// WWW-Authenticate challenge, checks that Resource is the endpoint it was
// trying to reach, and follows AuthorizationServers to discover where to
// authorize.
type ProtectedResourceMetadata struct {
	// Resource is the resource identifier: the absolute URL of the MCP
	// endpoint this document describes.
	Resource string `json:"resource"`
	// AuthorizationServers lists the issuer identifiers a client may authorize
	// against for this resource.
	AuthorizationServers []string `json:"authorization_servers"`
	// ScopesSupported lists the scopes the resource understands, one element
	// per scope. It is optional (RFC 9728 2) and omitted when the
	// configuration names no scope a client could request.
	ScopesSupported []string `json:"scopes_supported,omitempty"`
}

// AuthorizationServerMetadata describes this application's authorization server
// (RFC 8414). It is published only when the application is the authorization
// server; a resource server fronting somebody else's issuer points clients at
// that issuer through ProtectedResourceMetadata instead.
type AuthorizationServerMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint,omitempty"`
	ResponseTypesSupported        []string `json:"response_types_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	// ScopesSupported lists the scopes this server issues, one element per
	// scope. Optional under RFC 8414 2 and omitted when none is configured.
	ScopesSupported     []string `json:"scopes_supported,omitempty"`
	GrantTypesSupported []string `json:"grant_types_supported"`
	// TokenEndpointAuthMethodsSupported lists the client authentication methods
	// the token endpoint accepts. Omitted when none is known, which RFC 8414 2
	// has a client read as client_secret_basic.
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	// AuthorizationResponseIssParameterSupported states that authorization
	// responses carry the iss parameter (RFC 9207 2.3). Absent means false.
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported,omitempty"`
	// ClientIDMetadataDocumentSupported states that a client ID metadata
	// document URL is accepted as a client_id. Absent means false.
	ClientIDMetadataDocumentSupported bool `json:"client_id_metadata_document_supported,omitempty"`
}

// protectedResource builds the metadata document for the resource served at
// path under origin. path is the already-encoded path of the MCP endpoint,
// recovered from the well-known URL; the root resource is described by an empty
// path.
func (cfg Config) protectedResource(origin, path string) ProtectedResourceMetadata {
	return ProtectedResourceMetadata{
		Resource:             origin + path,
		AuthorizationServers: []string{cfg.issuerFor(origin)},
		ScopesSupported:      cfg.scopeTokens(),
	}
}

// authorizationServer builds this application's authorization-server metadata
// against origin. The registration endpoint is advertised only when a
// ClientStore backs it, so a client is never pointed at an endpoint that is not
// mounted.
func (cfg Config) authorizationServer(origin string) AuthorizationServerMetadata {
	meta := AuthorizationServerMetadata{
		Issuer:                        cfg.issuerFor(origin),
		AuthorizationEndpoint:         absolute(origin, cfg.AuthorizationEndpoint),
		TokenEndpoint:                 absolute(origin, cfg.TokenEndpoint),
		ResponseTypesSupported:        []string{responseTypeCode},
		CodeChallengeMethodsSupported: []string{codeChallengeMethodS256},
		ScopesSupported:               cfg.scopeTokens(),
		GrantTypesSupported:           []string{grantAuthorizationCode, grantRefreshToken},

		TokenEndpointAuthMethodsSupported:          cfg.tokenEndpointAuthMethods(),
		AuthorizationResponseIssParameterSupported: cfg.AuthorizationResponseIssuer,
		ClientIDMetadataDocumentSupported:          cfg.ClientIDMetadataDocuments,
	}
	if cfg.Clients != nil {
		meta.RegistrationEndpoint = origin + cfg.registerPath()
	}
	return meta
}

// tokenEndpointAuthMethods returns the client authentication methods to
// advertise: the configured ones, otherwise "none" when this application
// registers clients dynamically, since every client it registers is a public
// one, and nothing when it does not.
func (cfg Config) tokenEndpointAuthMethods() []string {
	methods := make([]string, 0, len(cfg.TokenEndpointAuthMethods))
	for _, method := range cfg.TokenEndpointAuthMethods {
		if method != "" && !contains(methods, method) {
			methods = append(methods, method)
		}
	}
	if len(methods) == 0 && cfg.Clients != nil {
		return []string{tokenEndpointAuthNone}
	}
	if len(methods) == 0 {
		return nil
	}
	return methods
}

// issuerFor returns the advertised issuer identifier given an already-resolved
// origin, so both metadata documents agree on it without re-deriving it.
//
// A configured AuthorizationServer is emitted byte for byte. RFC 8414 3.3
// requires the issuer a client discovers here to be identical to the issuer in
// the authorization server's own metadata, and a trailing slash is part of the
// identifier for issuers that publish one, so normalizing it here would break
// the comparison a client is required to make.
func (cfg Config) issuerFor(origin string) string {
	if cfg.AuthorizationServer != "" {
		return cfg.AuthorizationServer
	}
	return origin
}

// escapePath percent-encodes each segment of an already-normalized resource
// path so the advertised identifier is a syntactically valid URL even when the
// resource path carries characters that must be encoded. Ordinary path
// segments pass through unchanged, so the identifier still matches the URL the
// client requested.
func escapePath(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
