package client

import (
	"strings"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// ProtocolVersion is an MCP protocol version string (date based).
type ProtocolVersion = string

// Protocol versions this client can negotiate, newest first. The set is
// deliberately narrower than the set of versions that exist: a version is
// listed only once the client implements its wire rules.
const (
	// ProtocolV20260728 negotiates with a server/discover request and carries
	// the protocol metadata in the _meta member of every request.
	ProtocolV20260728 ProtocolVersion = "2026-07-28"
	// ProtocolV20251125 negotiates with an initialize request.
	ProtocolV20251125 ProtocolVersion = server.ProtocolV20251125
	// ProtocolV20250618 negotiates with an initialize request.
	ProtocolV20250618 ProtocolVersion = server.ProtocolV20250618
)

// LatestProtocolVersion is the newest version this client offers. It is the
// version the connection probe offers before anything is known about the
// server.
const LatestProtocolVersion = ProtocolV20260728

// Error codes a server uses to reject a discovery-era request. They identify a
// server that speaks the discovery handshake: a server that does not would
// answer such a request with a method-not-found (or another generic) error
// instead.
const (
	// CodeHeaderMismatch reports that a mirrored request header is missing or
	// does not match the request body.
	CodeHeaderMismatch = -32020
	// CodeMissingRequiredClientCapability reports that the request omitted a
	// client capability the server requires.
	CodeMissingRequiredClientCapability = -32021
	// CodeUnsupportedProtocolVersion reports that the offered protocol version
	// is not supported. Its data member may carry a "supported" list the client
	// can negotiate down to.
	CodeUnsupportedProtocolVersion = -32022
)

// handshake is the way a protocol version establishes a connection.
type handshake int

const (
	// handshakeInitialize negotiates with an initialize request followed by the
	// notifications/initialized notification.
	handshakeInitialize handshake = iota
	// handshakeDiscovery negotiates with a server/discover request; there is no
	// session and no initialized notification.
	handshakeDiscovery
)

// handshakeFor reports how a protocol version negotiates a connection.
func handshakeFor(version ProtocolVersion) handshake {
	if version == ProtocolV20260728 {
		return handshakeDiscovery
	}
	return handshakeInitialize
}

// livenessMethod names the request that asks a server whether it is still
// answering. Which request that is belongs to the revision: the initialize-era
// revisions define the ping request, and 2026-07-28 removed it, leaving
// server/discover as the one request every server of that revision must serve.
// Asking a modern server for a request it no longer defines would report a
// healthy server as one that does not answer.
func livenessMethod(version ProtocolVersion) string {
	if handshakeFor(version) == handshakeDiscovery {
		return "server/discover"
	}
	return "ping"
}

// clientSupportedVersions lists every version this client negotiates, in
// preference order (newest first).
func clientSupportedVersions() []ProtocolVersion {
	return []ProtocolVersion{ProtocolV20260728, ProtocolV20251125, ProtocolV20250618}
}

// initializeSupportedVersions lists the versions this client accepts as the
// outcome of an initialize handshake. The discovery-era version is not among
// them: it is never settled through initialize.
func initializeSupportedVersions() []ProtocolVersion {
	return []ProtocolVersion{ProtocolV20251125, ProtocolV20250618}
}

// preferredProtocolVersion returns the newest version this client supports that
// also appears in versions, reporting false when the sets do not intersect.
func preferredProtocolVersion(versions ...string) (ProtocolVersion, bool) {
	for _, supported := range clientSupportedVersions() {
		for _, offered := range versions {
			if offered == supported {
				return supported, true
			}
		}
	}
	return "", false
}

// supportsProtocolVersion reports whether this client can negotiate version.
func supportsProtocolVersion(version ProtocolVersion) bool {
	_, ok := preferredProtocolVersion(version)
	return ok
}

// supportsInitializeVersion reports whether version may be settled through an
// initialize handshake.
func supportsInitializeVersion(version ProtocolVersion) bool {
	for _, supported := range initializeSupportedVersions() {
		if version == supported {
			return true
		}
	}
	return false
}

// joinVersions renders a version list for an error message.
func joinVersions(versions []string) string {
	return strings.Join(versions, ", ")
}

// identifiesDiscoveryServer reports whether a JSON-RPC error came from a server
// that speaks the discovery handshake and rejected the request on its own
// terms. Such a rejection must not be retried as a legacy handshake.
func identifiesDiscoveryServer(err *jsonrpc.Error) bool {
	switch err.Code {
	case CodeHeaderMismatch, CodeMissingRequiredClientCapability, CodeUnsupportedProtocolVersion:
		return true
	default:
		return false
	}
}
