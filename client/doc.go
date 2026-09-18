// Package client is an MCP client for talking to MCP servers over stdio or
// streamable HTTP. It mirrors the server package: a Client connects, negotiates
// a protocol version, and then lists and invokes the server's tools, resources,
// and prompts.
//
// # Negotiation
//
// A connection is negotiated rather than assumed. The client probes with a
// server/discover request offering the newest version it speaks; a server that
// answers settles on the newest version both sides support. A server that does
// not know the method (or an endpoint that will not carry the request at all)
// sends the client back to the initialize handshake, which is what servers
// before the discovery era speak. A server that rejects the probe on protocol
// terms, naming the versions it supports, is retried at the newest mutual one.
//
// The handshake that settled a connection is remembered, so a reconnect goes
// straight to it, and WithProtocolVersion pins a version and skips the probe
// entirely.
//
// On a discovery-era connection every request carries the protocol metadata in
// its _meta member, and a transport with a header channel also mirrors the
// method and the addressed primitive into request headers, together with the
// input properties a tool's schema annotates for mirroring. An initialize-era
// connection behaves as it always has, session header included.
//
// WithClientCapabilities declares what the client can do for the server. A
// server only asks for the inputs of an unfinished result for a capability the
// client declared, so elicitation, sampling, and roots start there. The
// declaration travels on a discovery-era connection alone: an initialize-era
// server would ask for those inputs with a request of its own, which this
// client does not serve.
//
// # What the client keeps
//
// Two things a server says are kept past the call that read them: the result a
// connection was settled with, and the input properties the tool definitions of
// a listing ask to be mirrored into headers. Both are kept on the server's
// terms. On a discovery-era connection each states how long it may be
// considered fresh (ttlMs), and is read again the next time it is needed once
// that has run out; a result that states no lifetime has none, and is read
// again every time. A notification that the list of tools has changed outdates
// the definitions at once.
//
// Both also belong to the credential they were fetched with, because what a
// server answers may depend on who is asking. A WebClient whose bearer token
// changes, whether it is set again or a token callback answers with another
// one, negotiates again under the new token with its next request, and nothing
// the server told the old one is reused or shown. A custom transport that
// presents a credential of its own takes part by implementing
// AuthorizationAware.
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
