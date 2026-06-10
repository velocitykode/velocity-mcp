package server

// ProtocolVersion is an MCP protocol version string (date-based), mirroring
// laravel/mcp's ProtocolVersion enum. The supported set is advertised during
// the initialize handshake and used to negotiate the version with the client.
type ProtocolVersion = string

// Supported MCP protocol versions, newest first. These mirror laravel/mcp's
// Enums\ProtocolVersion cases. The first element is the latest and is used as
// the fallback when a client does not request a specific version.
const (
	ProtocolV20251125 ProtocolVersion = "2025-11-25"
	ProtocolV20250618 ProtocolVersion = "2025-06-18"
	ProtocolV20250326 ProtocolVersion = "2025-03-26"
	ProtocolV20241105 ProtocolVersion = "2024-11-05"
)

// LatestProtocolVersion is the newest protocol version this server speaks.
const LatestProtocolVersion = ProtocolV20251125

// supportedProtocolVersions returns the default ordered list of protocol
// versions the server supports, newest first. The order matters: the first
// element is the negotiation fallback (see methods.Initialize), matching
// laravel's ProtocolVersion::supported() ordering.
func supportedProtocolVersions() []ProtocolVersion {
	return []ProtocolVersion{
		ProtocolV20251125,
		ProtocolV20250618,
		ProtocolV20250326,
		ProtocolV20241105,
	}
}

// Capability keys advertised to clients during initialize, mirroring
// laravel/mcp's Server::CAPABILITY_* constants.
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
)
