// Package oauth implements the client-side OAuth 2.0 authorization flows an MCP
// client uses to obtain a bearer token for a protected MCP server, following
// the MCP authorization specification: protected-resource and
// authorization-server metadata discovery (RFC 9728 / RFC 8414), PKCE
// (RFC 7636), dynamic client registration (RFC 7591), and the
// authorization-code, refresh-token, and client-credentials grants.
//
// The package is a leaf relative to the client: it never imports the client
// package. It uses velocity's httpclient for all network calls (so SSRF
// guards, redirect-header stripping, and TLS minimums apply) and velocity's str
// for random state generation.
//
// Unlike a server-session-bound flow, the authorization-code dance here is
// explicit: AuthorizationURL returns the URL to send the user to together with
// a PendingAuthorization value that the caller persists (in their own session
// store) and hands back to ExchangeCode when the provider redirects to the
// callback. This keeps the package free of any ambient request/session state
// and makes every flow directly testable.
package oauth
