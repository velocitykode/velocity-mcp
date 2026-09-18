package server

// ProtocolVersion is an MCP protocol version string (date-based). The supported
// set is advertised by the server/discover method and is what a request's
// protocol metadata is validated against.
type ProtocolVersion = string

// MCP protocol versions, newest first.
const (
	ProtocolV20260728 ProtocolVersion = "2026-07-28"
	ProtocolV20251125 ProtocolVersion = "2025-11-25"
	ProtocolV20250618 ProtocolVersion = "2025-06-18"
	ProtocolV20250326 ProtocolVersion = "2025-03-26"
	ProtocolV20241105 ProtocolVersion = "2024-11-05"
)

// LatestProtocolVersion is the newest protocol version this server speaks.
const LatestProtocolVersion = ProtocolV20260728

// ProtocolHandshake names the opening exchange a protocol version uses. Every
// version belongs to exactly one of the two families, so the handshake a client
// chose tells the server which rules the rest of the connection follows.
type ProtocolHandshake int

const (
	// HandshakeInitialize is the "initialize" request/response handshake used
	// by revisions up to 2025-11-25. Its requests carry no protocol metadata in
	// params._meta and no mirrored MCP HTTP headers.
	HandshakeInitialize ProtocolHandshake = iota
	// HandshakeDiscovery is the "server/discover" handshake introduced in
	// 2026-07-28. Every request restates the protocol version and the client
	// capabilities in its own params._meta.
	HandshakeDiscovery
)

// String renders the handshake kind for diagnostics.
func (h ProtocolHandshake) String() string {
	switch h {
	case HandshakeDiscovery:
		return "discovery"
	case HandshakeInitialize:
		return "initialize"
	default:
		return "unknown"
	}
}

// HandshakeFor returns the handshake kind a protocol version uses. An unknown
// version reports HandshakeInitialize, because a client that does not speak a
// revision this server knows cannot have opened with discovery.
func HandshakeFor(version ProtocolVersion) ProtocolHandshake {
	if version == ProtocolV20260728 {
		return HandshakeDiscovery
	}
	return HandshakeInitialize
}

// ServerSupportedVersions returns the protocol versions the server advertises
// through server/discover and accepts in a request's protocol metadata.
func ServerSupportedVersions() []ProtocolVersion {
	return []ProtocolVersion{ProtocolV20260728}
}

// InitializeSupportedVersions returns the versions the legacy initialize
// handshake negotiates over, newest first. The first element is the version
// offered to a client that asks for anything outside the list. The discovery
// revision is deliberately absent: a client that speaks it opens with
// server/discover, not initialize.
func InitializeSupportedVersions() []ProtocolVersion {
	return []ProtocolVersion{ProtocolV20251125, ProtocolV20250618}
}

// supportedProtocolVersions returns the default list a Server advertises. It is
// the server-supported set unless WithProtocolVersions overrides it.
func supportedProtocolVersions() []ProtocolVersion {
	return ServerSupportedVersions()
}

// Capability keys advertised to clients during the handshake.
const (
	// CapabilityTools is the "tools" capability key.
	CapabilityTools = "tools"
	// CapabilityResources is the "resources" capability key.
	CapabilityResources = "resources"
	// CapabilityPrompts is the "prompts" capability key.
	CapabilityPrompts = "prompts"
	// CapabilityCompletions is the "completions" capability key. It is not
	// enabled by default; a server opts in via WithCapability.
	CapabilityCompletions = "completions"
	// CapabilityExtensions is the "extensions" capability key, the object under
	// which protocol extensions (such as MCP Apps) are advertised.
	CapabilityExtensions = "extensions"
)
