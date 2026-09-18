// Package oauth is the resource-server side of OAuth for an MCP server mounted
// on the velocity router: the bearer challenge that tells a client where to
// authorize, the discovery documents that describe this resource and its
// authorization server, and an optional dynamic client registration endpoint.
//
// It implements the authorization pieces the MCP specification delegates to
// OAuth 2.1:
//
//   - a 401 on a protected MCP route carries a WWW-Authenticate Bearer
//     challenge naming the protected-resource metadata document (RFC 9728 5.1),
//     with error="invalid_token" when the request presented a token and without
//     an error code when it presented none (RFC 6750 3),
//   - a guard that refuses a token for lacking scope states so on its 403 through
//     InsufficientScope (RFC 6750 3.1), naming the scope the call needs,
//   - GET /.well-known/oauth-protected-resource[/<resource path>] serves
//     protected-resource metadata (RFC 9728),
//   - GET /.well-known/oauth-authorization-server[/<path>] serves
//     authorization-server metadata (RFC 8414) when this application also is
//     the authorization server,
//   - POST <prefix>/register performs dynamic client registration (RFC 7591)
//     against an application-supplied ClientStore.
//
// The package authenticates nothing. Whatever velocity middleware guards the
// MCP route decides who the caller is and rejects with 401; this package only
// makes that rejection actionable and publishes the metadata a client needs to
// recover from it. It depends on the velocity router and validation engine
// only, so mounting it pulls in no persistence layer: an application that wants
// dynamic registration implements ClientStore over whatever it already uses.
package oauth
