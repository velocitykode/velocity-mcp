package client

import (
	"context"
	"errors"
	"time"

	"github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// negotiatedConnection is the outcome of a successful handshake: the settled
// protocol version and the result that settled it. Exactly one of the two
// results is set, depending on which handshake the server speaks.
//
// staleAfter is the moment a discover result stops being fresh. The
// specification makes it a cacheable result like a listing, with a lifetime the
// server states on it, so what it advertises is kept for that long and read
// again the next time it is asked for after that. An initialize result states
// no lifetime and has none: what it advertises was agreed for the session it
// opened, and stands for as long as the session does.
//
// authorization is the authorization context the frames of the handshake
// presented. What a server advertises may depend on who is asking, so the
// result belongs to that context and is shown in no other.
type negotiatedConnection struct {
	version       ProtocolVersion
	discover      *DiscoverResult
	initialize    *InitializeResult
	staleAfter    time.Time
	authorization int64
}

// remembered returns what is kept of a connection once the credential it was
// settled under is no longer the one presented: the version that says which
// handshake its server speaks, and nothing the server told the caller who was
// asking then.
func (c *negotiatedConnection) remembered() *negotiatedConnection {
	return &negotiatedConnection{version: c.version, authorization: c.authorization}
}

// outlived reports whether what the connection advertises has run out of the
// lifetime the server gave it.
func (c *negotiatedConnection) outlived(now time.Time) bool {
	return c != nil && c.discover != nil && !now.Before(c.staleAfter)
}

// discoverResult returns the discover result, or nil when the connection was
// settled through initialize.
func (c *negotiatedConnection) discoverResult() *DiscoverResult {
	if c == nil {
		return nil
	}
	return c.discover
}

// initializeResult returns the initialize result, or nil when the connection
// was settled through discovery.
func (c *negotiatedConnection) initializeResult() *InitializeResult {
	if c == nil {
		return nil
	}
	return c.initialize
}

// capabilities returns the server capabilities, whichever handshake reported
// them.
func (c *negotiatedConnection) capabilities() map[string]any {
	switch {
	case c == nil:
		return nil
	case c.discover != nil:
		return c.discover.Capabilities
	case c.initialize != nil:
		return c.initialize.Capabilities
	default:
		return nil
	}
}

// serverInfo returns the server identity, whichever handshake reported it.
func (c *negotiatedConnection) serverInfo() schema.Implementation {
	switch {
	case c == nil:
		return schema.Implementation{}
	case c.discover != nil:
		return c.discover.ServerInfo
	case c.initialize != nil:
		return c.initialize.ServerInfo
	default:
		return schema.Implementation{}
	}
}

// instructions returns the server instructions, whichever handshake reported
// them.
func (c *negotiatedConnection) instructions() string {
	switch {
	case c == nil:
		return ""
	case c.discover != nil:
		return c.discover.Instructions
	case c.initialize != nil:
		return c.initialize.Instructions
	default:
		return ""
	}
}

// handshake negotiates a connection. A pinned version is offered on its own
// terms; otherwise the handshake the server is already known to speak is tried
// first, and anything else goes through the probe. The connection it settles is
// recorded under authorization, the context the frames it sends present. The
// caller holds the exchange lock.
func (p *protocol) handshake(ctx context.Context, authorization int64) error {
	pinned, err := p.pinnedVersion()
	if err != nil {
		return err
	}

	if pinned != "" {
		conn, err := p.negotiatePinned(ctx, pinned)
		if err != nil {
			return err
		}
		p.settleOn(conn, authorization)
		return nil
	}

	if remembered := p.connection(); remembered != nil && handshakeFor(remembered.version) == handshakeInitialize {
		conn, err := p.initialize(ctx, remembered.version, false)
		if err == nil {
			p.settleOn(conn, authorization)
			return nil
		}
		if isAuthorizationError(err) || ctx.Err() != nil {
			return err
		}
		// The server no longer answers the handshake it answered before: forget
		// what was remembered and negotiate from scratch over a fresh channel.
		p.setConnection(nil)
		if err := p.transport.Connect(ctx); err != nil {
			return err
		}
	}

	conn, err := p.probe(ctx)
	if err != nil {
		return err
	}
	p.settleOn(conn, authorization)
	return nil
}

// settleOn records the connection a handshake settled, under the authorization
// context its frames presented. The connection is stamped before it is
// published, so nothing ever reads one that says it belongs to no context.
func (p *protocol) settleOn(conn *negotiatedConnection, authorization int64) {
	conn.authorization = authorization
	p.setConnection(conn)
}

// negotiatePinned negotiates the pinned version with the handshake that version
// belongs to, and accepts nothing else.
func (p *protocol) negotiatePinned(ctx context.Context, pinned ProtocolVersion) (*negotiatedConnection, error) {
	if handshakeFor(pinned) == handshakeDiscovery {
		return p.discover(ctx, pinned)
	}
	return p.initialize(ctx, pinned, true)
}

// probe negotiates with an unknown server: it offers discovery first and falls
// back to the initialize handshake when the answer says the server does not
// speak it.
//
// Only a rejection that identifies a discovery-era server stops the fallback;
// it is either negotiated down to a mutually supported version or surfaced.
// Every other JSON-RPC error means the server is one that predates the
// handshake, because the fallback must not be keyed to any one error code:
// servers answer an unknown request made before initialize with an
// implementation-defined error (method-not-found and invalid-params are the
// common ones) or with nothing at all. A failure that is not the server's
// answer, such as a discover result this client cannot read, is surfaced: the
// server did reply on discovery's terms.
func (p *protocol) probe(ctx context.Context) (*negotiatedConnection, error) {
	conn, err := p.discover(ctx, "")
	if err == nil {
		return conn, nil
	}

	// The rejection is remembered so a failing fallback can report both halves
	// of the story rather than only the second one.
	var rejection error
	var rpcErr *jsonrpc.Error
	var transportErr *TransportError
	switch {
	case errors.As(err, &rpcErr):
		if identifiesDiscoveryServer(rpcErr) {
			return p.negotiateDown(ctx, rpcErr)
		}
		rejection = err
	case errors.As(err, &transportErr):
		rejection = err
	default:
		return nil, err
	}

	// A context that is already done cannot carry a second handshake: the
	// fallback would only spend a reconnect to fail the same way, so the
	// rejection the probe already has is the answer.
	if ctx.Err() != nil {
		return nil, err
	}

	if err := p.transport.Connect(ctx); err != nil {
		return nil, err
	}

	conn, err = p.fallback(ctx)
	if err == nil {
		return conn, nil
	}
	if isAuthorizationError(err) || rejection == nil {
		return nil, err
	}
	return nil, wrapError(err, rejection.Error()+"; the legacy handshake also failed")
}

// fallback runs the initialize handshake the probe fell back to, once over the
// channel the probe was answered on and once more over a fresh one when that
// channel turns out to be gone.
//
// A server that predates the discovery request may answer it on protocol terms
// and then end. Its refusal is not a failure of the channel, so the probe keeps
// the channel and the transport reports itself connected; only the next frame
// finds the peer gone. Reconnecting after such a failure is what starts the
// server again, so the handshake it does speak is tried over a live channel
// rather than being reported against a corpse.
//
// Only a failure of the channel is retried. A timeout is not: the server took
// the frame and owes a reply, so starting it again would only spend a second
// wait to fail the same way. Neither is a refusal on protocol terms, which is
// an answer, nor a missing credential, which no reconnect supplies.
func (p *protocol) fallback(ctx context.Context) (*negotiatedConnection, error) {
	conn, err := p.initialize(ctx, ProtocolV20251125, false)
	if err == nil {
		return conn, nil
	}

	var timeoutErr *TimeoutError
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || errors.As(err, &timeoutErr) ||
		isAuthorizationError(err) || ctx.Err() != nil {
		return nil, err
	}

	// The channel is torn down before it is rebuilt: Connect is idempotent, so
	// a transport that has not noticed the peer is gone would otherwise hand
	// back the same dead channel.
	_ = p.transport.Disconnect()
	if cerr := p.transport.Connect(ctx); cerr != nil {
		return nil, err
	}
	return p.initialize(ctx, ProtocolV20251125, false)
}

// negotiateDown retries the handshake at the newest version both peers support,
// taken from the rejection's data. The rejection is surfaced when there is no
// such version, or when it is one that discovery would have settled anyway.
func (p *protocol) negotiateDown(ctx context.Context, rpcErr *jsonrpc.Error) (*negotiatedConnection, error) {
	version, ok := preferredProtocolVersion(supportedVersionsFromData(rpcErr.Data)...)
	if !ok || handshakeFor(version) != handshakeInitialize {
		return nil, rpcErr
	}
	return p.initialize(ctx, version, false)
}

// supportedVersionsFromData reads the versions a server advertised in the data
// member of an unsupported-version rejection.
func supportedVersionsFromData(data any) []string {
	container, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	entries, ok := container["supported"].([]any)
	if !ok {
		return nil
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if version, ok := entry.(string); ok {
			versions = append(versions, version)
		}
	}
	return versions
}

// initialize performs the initialize handshake at the offered version and
// announces the settled version with the initialized notification. When the
// version was pinned, a server that settles on another one is refused.
//
// The handshake declares no capabilities of its own. A revision settled this
// way asks a client for elicitation, sampling, or roots by sending it a request
// of its own, which this client answers with method-not-found and an HTTP
// transport cannot receive at all; declaring one here would invite exactly the
// requests it cannot serve. What a caller declares travels on the revision that
// asks for those inputs as part of the result the request is waiting on.
func (p *protocol) initialize(ctx context.Context, version ProtocolVersion, pinned bool) (*negotiatedConnection, error) {
	raw, err := p.attempt(ctx, "initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    noCapabilities,
		"clientInfo":      p.identity().ToMap(),
	}, version, nil)
	if err != nil {
		return nil, err
	}

	result, err := parseInitializeResult(raw)
	if err != nil {
		return nil, err
	}
	if pinned && result.ProtocolVersion != version {
		return nil, versionMismatch(result.ProtocolVersion, version)
	}
	if err := p.notify(ctx, "notifications/initialized", result.ProtocolVersion); err != nil {
		return nil, err
	}
	return &negotiatedConnection{version: result.ProtocolVersion, initialize: result}, nil
}

// discover performs the discovery handshake, offering the pinned version or the
// latest one this client speaks. The settled version is the newest both peers
// support; when that version belongs to the initialize handshake the connection
// is settled by initializing at it.
func (p *protocol) discover(ctx context.Context, pinned ProtocolVersion) (*negotiatedConnection, error) {
	offered := pinned
	if offered == "" {
		offered = LatestProtocolVersion
	}

	raw, err := p.attempt(ctx, "server/discover", nil, offered, nil)
	if err != nil {
		return nil, err
	}
	receivedAt := p.now()
	result, err := parseDiscoverResult(raw)
	if err != nil {
		return nil, err
	}

	settled, ok := preferredProtocolVersion(result.SupportedVersions...)
	if !ok {
		return nil, newError("the server supports protocol versions [" + joinVersions(result.SupportedVersions) +
			"]; this client supports [" + joinVersions(clientSupportedVersions()) + "]")
	}
	if pinned != "" && settled != pinned {
		return nil, versionMismatch(settled, pinned)
	}
	if handshakeFor(settled) == handshakeInitialize {
		return p.initialize(ctx, settled, false)
	}
	return &negotiatedConnection{version: settled, discover: result, staleAfter: staleAfter(receivedAt, raw)}, nil
}

// versionMismatch reports a server settling on a version other than the one
// that was pinned.
func versionMismatch(settled, requested ProtocolVersion) error {
	return newError("the server settled on protocol version [" + settled +
		"] while [" + requested + "] was requested")
}

// isAuthorizationError reports whether a handshake failed because the server
// requires authorization. Such a failure is never retried with another
// handshake: the credentials, not the protocol, are what is missing.
func isAuthorizationError(err error) bool {
	var required *oauth.AuthorizationRequiredError
	if errors.As(err, &required) {
		return true
	}
	var oauthErr *oauth.Error
	return errors.As(err, &oauthErr)
}
