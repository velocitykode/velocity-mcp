package event

import "time"

// Event identifier constants. These are stable, dotted names used to identify
// MCP events when dispatched through Velocity's event system. They must not
// change once published: external listeners key off them.
const (
	// NameSessionInitialized identifies the SessionInitialized event.
	NameSessionInitialized = "mcp.session.initialized"
	// NameToolCalled identifies the ToolCalled event.
	NameToolCalled = "mcp.tool.called"
	// NameToolFailed identifies the ToolFailed event.
	NameToolFailed = "mcp.tool.failed"
)

// ClientInfo describes the MCP client that connected to the server, as reported
// during the initialize handshake. All fields are optional: a client may omit
// any of them.
//
// See https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle#initialization
type ClientInfo struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// SessionInitialized is dispatched once an MCP session has completed the
// initialize handshake. It mirrors laravel/mcp's SessionInitialized event.
//
// ClientInfo and ClientCapabilities are nil when the client did not supply
// them. Use the ClientName, ClientTitle and ClientVersion helpers to read
// client metadata without nil-checking ClientInfo.
type SessionInitialized struct {
	// SessionID is the server-assigned identifier for the session.
	SessionID string
	// ClientInfo is the client implementation metadata, or nil if absent.
	ClientInfo *ClientInfo
	// ProtocolVersion is the negotiated MCP protocol version, or "" if absent.
	ProtocolVersion string
	// ClientCapabilities is the raw capabilities object the client advertised,
	// or nil if absent.
	ClientCapabilities map[string]any
}

// Name returns the stable event identifier.
func (e SessionInitialized) Name() string { return NameSessionInitialized }

// ClientName returns the client name from ClientInfo, or "" if unavailable.
func (e SessionInitialized) ClientName() string {
	if e.ClientInfo == nil {
		return ""
	}
	return e.ClientInfo.Name
}

// ClientTitle returns the client title from ClientInfo, or "" if unavailable.
func (e SessionInitialized) ClientTitle() string {
	if e.ClientInfo == nil {
		return ""
	}
	return e.ClientInfo.Title
}

// ClientVersion returns the client version from ClientInfo, or "" if unavailable.
func (e SessionInitialized) ClientVersion() string {
	if e.ClientInfo == nil {
		return ""
	}
	return e.ClientInfo.Version
}

// ToolCalled is dispatched after an MCP tool has been invoked and returned a
// result. It is fired for both successful results and tool-level error results
// (results with isError set); a transport- or protocol-level failure that
// prevents the tool from running is reported via ToolFailed instead.
type ToolCalled struct {
	// SessionID is the identifier of the session that issued the call.
	SessionID string
	// Tool is the name of the invoked tool.
	Tool string
	// Arguments is the argument map supplied with the call, or nil if none.
	Arguments map[string]any
	// IsError reports whether the tool returned an error result.
	IsError bool
	// Duration is the wall-clock time the tool took to handle the call.
	Duration time.Duration
}

// Name returns the stable event identifier.
func (e ToolCalled) Name() string { return NameToolCalled }

// ToolFailed is dispatched when an MCP tool invocation fails before producing a
// result, for example because the tool handler returned a non-nil error or the
// named tool could not be resolved.
type ToolFailed struct {
	// SessionID is the identifier of the session that issued the call.
	SessionID string
	// Tool is the name of the tool that failed.
	Tool string
	// Arguments is the argument map supplied with the call, or nil if none.
	Arguments map[string]any
	// Err is the underlying failure. It is intended for server-side logging and
	// must not be leaked verbatim to MCP clients.
	Err error
	// Duration is the wall-clock time spent before the failure was observed.
	Duration time.Duration
}

// Name returns the stable event identifier.
func (e ToolFailed) Name() string { return NameToolFailed }
