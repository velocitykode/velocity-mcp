// Package client is an MCP client for talking to MCP servers over stdio or
// streamable HTTP. It mirrors the server package: a Client connects, negotiates
// the protocol version on initialize, and then lists and invokes the server's
// tools, resources, and prompts.
//
// Two constructors cover the common cases:
//
//	c := client.Local("server-binary", "--flag")   // spawn a subprocess, stdio transport
//	w := client.Web("https://example.com/mcp")      // streamable HTTP transport
//
// Local returns a *Client; Web returns a *WebClient, which adds bearer-token and
// OAuth helpers (see the client/oauth subpackage). Both connect lazily on the
// first call and clean up the transport on Disconnect.
//
// The package builds on velocity components: the HTTP transport uses velocity's
// httpclient (inheriting its SSRF guards, redirect-header stripping, and TLS
// minimums), and OAuth uses velocity's str for state generation. List operations
// transparently follow nextCursor pagination.
package client
