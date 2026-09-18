// Package mcpclient is the application-facing integration layer for consuming
// MCP servers from a Velocity web app. Where the client package is the
// low-level engine (transports, protocol, oauth), this package is the ergonomic
// front door: register named MCP servers once, then mount the OAuth
// authorization-code routes for any of them with a single module.
//
//	mcpclient.RegisterClient("example", "https://mcp.example.com/mcp")
//
//	func Configure(reg *velocity.ModuleRegistry) {
//	    reg.Add(mcpclient.OAuthRoutesFor("example", oauth.Config{
//	        ClientID: "veladmin", Issuer: "https://auth.example.com", Scope: "mcp:use",
//	    }))
//	}
//
// A pre-registered ClientID belongs to the authorization server it was
// registered with, so Issuer has to name that server: the flow is refused for
// any other one, whichever server the MCP server advertises. Leave both empty to
// identify the application by its client ID metadata document or by dynamic
// registration instead.
//
// OAuthRoutesFor mounts two routes on the session-backed web stack:
//
//	GET /mcp/oauth/{name}/redirect   begins the flow (discovery, PKCE, redirect)
//	GET /mcp/oauth/{name}/callback   completes it (code exchange, token storage)
//
// plus this application's client ID metadata document, which authorization
// servers fetch unauthenticated while resolving the client_id, so it is mounted
// without the web stack's session, CSRF and authentication middleware:
//
//	GET /mcp/oauth/{name}/client-metadata.json
//
// The redirect route first asks the MCP server, without credentials, for its
// challenge, and starts discovery from the protected-resource metadata URL and
// the scope that challenge names (RFC 9728 5.1). A server that names no
// metadata URL is discovered through the well-known locations instead: the one
// built from the path of its endpoint, then the one at the root.
//
// A server that advertises client_id_metadata_document_supported is handed that
// URL as the client_id, which makes the application a public client and avoids
// registering a new client record for every connection. Use WithClientMetadata
// to describe the application (client_name, logo_uri, client_uri, extra
// redirect_uris) and WithPublicURL to pin the origin the document is built from.
//
// After the user authorizes, the access token is persisted (velocity session by
// default) and any handler can obtain an authorized client:
//
//	w, _ := mcpclient.For(c, "example")  // *client.WebClient with bearer token
//	tools, _ := w.Tools(c.Request.Context())
//
// Token persistence is pluggable via WithStore / SetDefaultStore; the default
// SessionStore keeps tokens in the velocity session, MemoryStore offers a
// self-contained cookie-keyed fallback for apps without sessions.
package mcpclient
