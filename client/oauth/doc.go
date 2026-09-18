// Package oauth implements the client-side OAuth 2.0 authorization flows an MCP
// client uses to obtain a bearer token for a protected MCP server, following
// the MCP authorization specification: protected-resource and
// authorization-server metadata discovery (RFC 9728 / RFC 8414), PKCE
// (RFC 7636), client ID metadata documents, dynamic client registration
// (RFC 7591), and the authorization-code, refresh-token, and client-credentials
// grants.
//
// Two rules of the authorization specification shape the authorization-code
// flow. PKCE with S256 is mandatory: a server that does not advertise it is
// refused with an error wrapping ErrPKCERequired. And a client_id is resolved in
// a fixed order: the configured Config.ClientID, then Config.ClientIDMetadataURL
// when the server advertises client_id_metadata_document_supported (a public
// client, no secret), and only then dynamic registration.
//
// Credentials are bound to the authorization server they belong to. The
// protected resource is what advertises that server, so a resource that is
// compromised or hijacked can point a client at a server of its choosing, and
// nothing the resource says may decide where a credential travels. An issued
// TokenSet records its issuer and is refreshed against that server alone, and
// the credentials in Config are presented only to Config.Issuer, which is
// required whenever Config.ClientID or Config.ClientSecret is set. Both refusals
// wrap ErrIssuerMismatch, and configured credentials with no configured issuer
// wrap ErrIssuerRequired.
//
// The package is a leaf relative to the client: it never imports the client
// package. It uses velocity's httpclient for all network calls (so TLS minimums
// apply, redirects are not followed, and the dial-time guard refuses any host
// that resolves to a private or internal address unless Config.AllowPrivateHosts
// lifts it) and velocity's str for random state generation. Every URL in a flow
// but the resource URL is advertised by a server, which is why the guard sits
// on the connection and not on the spelling of the URL.
//
// Unlike a server-session-bound flow, the authorization-code dance here is
// explicit: AuthorizationURL returns the URL to send the user to together with
// a PendingAuthorization value that the caller persists (in their own session
// store) and hands back to ExchangeCode when the provider redirects to the
// callback. This keeps the package free of any ambient request/session state
// and makes every flow directly testable.
package oauth
