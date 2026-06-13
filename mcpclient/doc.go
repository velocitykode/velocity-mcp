// Package mcpclient is the application-facing integration layer for consuming
// MCP servers from a Velocity web app. Where the client package is the
// low-level engine (transports, protocol, oauth), this package is the ergonomic
// front door: register named MCP servers once, then mount the OAuth
// authorization-code routes for any of them with a single provider.
//
//	mcpclient.RegisterClient("example", "https://mcp.example.com/mcp")
//
//	func Configure(reg *velocity.ProviderRegistry) {
//	    reg.Add(mcpclient.OAuthRoutesFor("example", oauth.Config{
//	        ClientID: "veladmin", Scope: "mcp:use",
//	    }))
//	}
//
// OAuthRoutesFor mounts two routes on the session-backed web stack:
//
//	GET /mcp/oauth/{name}/redirect   begins the flow (discovery, PKCE, redirect)
//	GET /mcp/oauth/{name}/callback   completes it (code exchange, token storage)
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
