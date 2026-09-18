// Package server provides the MCP server core: the Server type, the
// primitives (Tool, Resource, Prompt), per-session context, and the
// registrar for exposing servers over transports (Web, Local).
//
// # Handshakes
//
// The server answers both opening exchanges the protocol defines. A client on
// revision 2026-07-28 or later opens with server/discover, reads the versions
// and capabilities it advertises, and then restates the protocol version and
// its own capabilities in the params._meta of every subsequent request; over
// HTTP it also mirrors those values in the MCP request headers. A client on an
// earlier revision opens with initialize, which negotiates over
// InitializeSupportedVersions, establishes a session, and is exempt from both
// the per-request metadata and the header mirroring.
//
// Which set of rules a request follows is decided per request, by whether it
// declares either protocol member in its params._meta, so the two kinds of
// client can be served side by side.
package server
