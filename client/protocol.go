package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// protocol drives the JSON-RPC exchange over a transport: it owns request-id
// allocation, the connection handshake and the negotiated protocol version, and
// the send-then-receive loop (including answering server-initiated pings). It
// is the client's internal engine; Client and WebClient are the public surface.
type protocol struct {
	transport Transport

	// now is the local clock the freshness of a kept result is measured on. It
	// is set once, when the protocol is built.
	now func() time.Time

	// notified is told the method of every notification the server sends, so
	// whoever keeps what the server stated hears when the server says it has
	// changed. It is set once, before the first exchange, and may be nil.
	notified func(method string)

	// released is run each time an exchange has let go of the gate, before the
	// caller that held it reads what the exchange returned. It is set once,
	// before the first exchange, and is nil outside tests: it is the one place
	// the interval between the end of an exchange and its caller going on can
	// be entered on purpose.
	released func()

	// exchange serializes whole request/response exchanges, including the
	// handshake, so concurrent callers cannot interleave frames on a transport
	// that carries one reply at a time. It is a one-slot channel rather than a
	// mutex because a caller waiting its turn has to be able to give up: the
	// exchange ahead of it holds the channel for a whole network round trip,
	// which a slow server, a custom transport, or a stream of notifications can
	// stretch indefinitely, and a request whose own context is done by then has
	// nothing left to wait for.
	exchange chan struct{}

	mu         sync.Mutex
	clientInfo schema.Implementation
	// clientCapabilities is the capability set this client advertises, encoded
	// once when it is set: a request renders it straight onto the wire, so the
	// caller's map is never read after the call that supplied it.
	clientCapabilities json.RawMessage
	capabilitiesErr    error
	connected          bool
	connecting         bool
	nextID             int64
	pinned             ProtocolVersion
	pinErr             error
	conn               *negotiatedConnection
	// generation counts the handshakes this protocol has settled. It tells one
	// connection from the next, so what was read over an earlier one is not
	// taken for what the server now states: a server that restarts between two
	// calls may answer with definitions it did not answer with before.
	generation int64
}

// noCapabilities is the capability set of a client that declares none.
var noCapabilities = json.RawMessage(`{}`)

// newProtocol builds a protocol over the given transport and client identity.
func newProtocol(transport Transport, clientInfo schema.Implementation) *protocol {
	return &protocol{
		transport:          transport,
		now:                time.Now,
		clientInfo:         clientInfo,
		clientCapabilities: noCapabilities,
		nextID:             1,
		exchange:           make(chan struct{}, 1),
	}
}

// acquireExchange takes the exchange gate, giving up when ctx is done. A
// context that is already done never takes it: a select over a free gate and a
// done context picks between them at random, and a request whose deadline has
// passed must not reach the wire.
func (p *protocol) acquireExchange(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return wrapError(err, "the request was abandoned before it was sent")
	}
	select {
	case p.exchange <- struct{}{}:
		return nil
	case <-ctx.Done():
		return wrapError(ctx.Err(), "the request was abandoned while waiting for the connection to be free")
	}
}

// releaseExchange hands the gate to the next caller waiting for it.
func (p *protocol) releaseExchange() {
	<-p.exchange
	if p.released != nil {
		p.released()
	}
}

// isConnected reports whether a connection has been negotiated.
func (p *protocol) isConnected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connected
}

// setClientInfo replaces the identity sent to servers. It takes effect on the
// next handshake.
func (p *protocol) setClientInfo(info schema.Implementation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clientInfo = info
}

// identity returns the identity this client sends to servers.
func (p *protocol) identity() schema.Implementation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clientInfo
}

// setClientCapabilities replaces the capability set this client advertises. It
// takes effect on the next handshake, and on the next request of a revision
// that states the capabilities on every one. A set that cannot be encoded is
// recorded and reported by every request that would have carried it, so a
// client never quietly advertises less than it was told to.
func (p *protocol) setClientCapabilities(capabilities map[string]any) {
	encoded, err := json.Marshal(capabilities)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.capabilitiesErr = newError("the client capabilities cannot be encoded as JSON")
		return
	}
	p.capabilitiesErr = nil
	if len(capabilities) == 0 {
		p.clientCapabilities = noCapabilities
		return
	}
	p.clientCapabilities = encoded
}

// declaredCapabilities returns the encoded capability set to put on the wire.
func (p *protocol) declaredCapabilities() (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.capabilitiesErr != nil {
		return nil, p.capabilitiesErr
	}
	return p.clientCapabilities, nil
}

// connection returns the negotiated connection, or nil when none has been
// negotiated. The connection outlives a disconnect: it is what lets a reconnect
// go straight to the handshake the server is known to speak.
func (p *protocol) connection() *negotiatedConnection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn
}

// setConnection records (or clears) the negotiated connection.
func (p *protocol) setConnection(conn *negotiatedConnection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn = conn
}

// initializeResult returns the initialize result of the negotiated connection,
// or nil when the connection was negotiated through discovery (or not at all).
func (p *protocol) initializeResult() *InitializeResult {
	return p.presentable().initializeResult()
}

// discoverResult returns the discover result of the negotiated connection, or
// nil when the connection was negotiated through initialize (or not at all).
func (p *protocol) discoverResult() *DiscoverResult {
	return p.presentable().discoverResult()
}

// presentable returns the negotiated connection when what it holds may be shown
// to whoever asks now: it was settled under the credential the transport would
// present at this moment. It sends nothing, so under another credential there
// is nothing of this caller's to show and it returns nil, which is what a
// connection that has not been negotiated returns.
func (p *protocol) presentable() *negotiatedConnection {
	conn := p.connection()
	if conn == nil || conn.authorization != p.authorizationNow() {
		return nil
	}
	return conn
}

// authorize settles the credential the frames of the exchange now starting
// present, and returns the authorization context it belongs to. A transport
// that presents no credential of its own has the one context. The caller holds
// the exchange lock.
func (p *protocol) authorize() int64 {
	if aware, ok := p.transport.(AuthorizationAware); ok {
		return aware.SettleAuthorization()
	}
	return 0
}

// authorizationNow returns the authorization context of the credential the
// transport would present now, settling nothing, so it may be asked without
// holding the exchange.
func (p *protocol) authorizationNow() int64 {
	if aware, ok := p.transport.(AuthorizationAware); ok {
		return aware.AuthorizationContext()
	}
	return 0
}

// pinProtocolVersion pins the protocol version the client offers, or clears the
// pin when version is empty. An unsupported version is recorded and reported by
// the next connect. Pinning discards the negotiated connection, so the next
// request negotiates again. It takes the exchange lock: a request already in
// flight finishes over the transport it started on rather than having it torn
// down underneath it.
func (p *protocol) pinProtocolVersion(version ProtocolVersion) {
	// Pinning is not a request and carries no context of its own, so it waits
	// for the gate however long the exchange in flight takes.
	_ = p.acquireExchange(context.Background())
	defer p.releaseExchange()

	p.mu.Lock()
	p.pinned = version
	p.pinErr = nil
	p.conn = nil
	if version != "" && !supportsProtocolVersion(version) {
		p.pinErr = newError("this client does not support protocol version [" + version +
			"]; it supports [" + joinVersions(clientSupportedVersions()) + "]")
	}
	connected := p.connected
	p.mu.Unlock()

	if connected {
		p.disconnect()
	}
}

// pinnedVersion returns the pinned protocol version, or the empty string when
// the client negotiates freely.
func (p *protocol) pinnedVersion() (ProtocolVersion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pinned, p.pinErr
}

// foundConnection identifies the connection a caller found standing: the
// version it was settled on, and the count that tells it from every other.
//
// Both are read while the exchange that settled or confirmed the connection is
// still held, for the reason the connection of a result is (see answered). What
// a caller decides from the version is a decision about that one connection, as
// which request asks a server of that revision whether it is alive, or that the
// revision mirrors nothing into headers. A version asked for once the gate is
// released may be that of a connection settled since, and one read without the
// count beside it says nothing of which connection the decision holds for.
type foundConnection struct {
	version    ProtocolVersion
	generation int64
}

// connect negotiates a connection and reports the one standing. It is
// idempotent.
func (p *protocol) connect(ctx context.Context) (foundConnection, error) {
	if err := p.acquireExchange(ctx); err != nil {
		return foundConnection{}, err
	}
	defer p.releaseExchange()

	conn, err := p.standingConnection(ctx, terms{})
	if err != nil {
		return foundConnection{}, err
	}
	return foundConnection{version: conn.version, generation: p.connectionGeneration()}, nil
}

// connectLocked negotiates a connection. The caller holds the exchange lock.
//
// Every exchange starts here, and it starts by settling the credential its
// frames present. A connection settled under another credential is not this
// caller's: the result it was settled with, the session it opened, and
// everything read over it were the server's answers to someone else, and the
// specification forbids reusing them across authorization contexts. It is given
// up and negotiated again under the credential now presented, which makes it a
// new connection to everything that dates what it keeps by the connection it
// was read over.
func (p *protocol) connectLocked(ctx context.Context) error {
	authorization := p.authorize()
	if standing := p.connection(); standing != nil && standing.authorization != authorization {
		p.forsake(standing)
	}
	if p.isConnected() {
		return nil
	}
	if _, err := p.pinnedVersion(); err != nil {
		return err
	}

	if err := p.transport.Connect(ctx); err != nil {
		return err
	}
	return p.settle(ctx, authorization)
}

// forsake gives up a connection settled under another credential than the one
// now presented. The transport is torn down, which releases the session with
// the credential that opened it, and what the connection held is dropped: only
// which handshake the server speaks is remembered.
func (p *protocol) forsake(standing *negotiatedConnection) {
	if p.isConnected() {
		p.disconnect()
	}
	p.setConnection(standing.remembered())
}

// settle runs the handshake over the channel now open and counts the connection
// it settles, which belongs to the authorization context the frames of the
// handshake present. The caller holds the exchange lock.
func (p *protocol) settle(ctx context.Context, authorization int64) error {
	p.setConnecting(true)
	err := p.handshake(ctx, authorization)
	p.setConnecting(false)
	if err != nil {
		p.disconnect()
		return err
	}

	p.mu.Lock()
	p.connected = true
	p.generation++
	p.mu.Unlock()
	return nil
}

// advertised returns the connection whose handshake result states what the
// server advertises, settling one first when none stands.
//
// A discover result is a cacheable result like any other: the server states how
// long it may be kept, and the specification has a stale one read again the next
// time it is needed, which is here. It is read again the way it was read the
// first time, by the handshake, over the channel that is already open: a server
// asked again may support other versions than it did, and settling on one is
// the handshake's to do. The connection that settles is a new one, so whatever
// was read over the one before is read again as well.
//
// A result this very call settled is as current as the server can state it,
// whatever lifetime it was given, so a server that lets nothing be kept is
// asked once for every call rather than twice.
//
// The connection is returned while the exchange is still held, so what the
// caller reads is the result this call saw settled rather than whichever one
// stands once the gate has been released.
func (p *protocol) advertised(ctx context.Context) (*negotiatedConnection, error) {
	if err := p.acquireExchange(ctx); err != nil {
		return nil, err
	}
	defer p.releaseExchange()

	standing := p.connectionGeneration()
	if err := p.connectLocked(ctx); err != nil {
		return nil, err
	}
	if conn := p.connection(); p.connectionGeneration() == standing && conn.outlived(p.now()) {
		// The handshake runs as it does when nothing stands: a failure of the
		// channel in the middle of it is the negotiation's to weigh, not a
		// reason to tear the channel down underneath it.
		p.mu.Lock()
		p.connected = false
		p.mu.Unlock()
		if err := p.settle(ctx, conn.authorization); err != nil {
			return nil, err
		}
	}
	return p.connection(), nil
}

// connectionGeneration returns the count of handshakes settled so far, which
// identifies the connection now standing. It is zero before the first one.
func (p *protocol) connectionGeneration() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generation
}

// setConnecting records whether a handshake is in flight. While it is, a
// request issued by the handshake itself must not try to connect again.
func (p *protocol) setConnecting(connecting bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connecting = connecting
}

// disconnect marks the protocol disconnected and tears down the transport. The
// negotiated connection is kept: it records which handshake this server speaks.
func (p *protocol) disconnect() {
	p.mu.Lock()
	p.connected = false
	p.mu.Unlock()
	_ = p.transport.Disconnect()
}

// disconnectIfConnected tears the transport down only once a connection has
// been negotiated. During the handshake the transport is left alone: the
// negotiation itself decides whether to retry over it.
func (p *protocol) disconnectIfConnected() {
	if p.isConnected() {
		p.disconnect()
	}
}

// dispatch sends a request over the negotiated connection and returns the raw
// result, connecting first when needed.
func (p *protocol) dispatch(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.dispatchWith(ctx, method, params, nil)
}

// dispatchWith is dispatch with extra request headers, which a discovery-era
// exchange sends alongside the mirrored ones. extra is called only when the
// negotiated version carries headers at all, so a call that cannot render them
// still goes out over a connection that would not have sent them, and it is
// handed the encoded params of the attempt it belongs to, so a header can only
// state what that attempt's body carries. If the server reports the session as
// expired the connection is renegotiated once and the request retried.
func (p *protocol) dispatchWith(ctx context.Context, method string, params any, extra headerFunc) (json.RawMessage, error) {
	reply, err := p.exchangeWith(ctx, method, params, terms{headers: extra})
	if err != nil {
		return nil, err
	}
	return reply.result, nil
}

// terms are what an exchange is held to beyond its method and params: the extra
// headers each attempt renders, and the authorization context it insists on
// travelling in, where zero insists on none. A request that carries something
// the server handed to one caller, as the cursor of a listing is, names the
// context it was handed out in, and is refused before it is sent when the
// credential now presented belongs to another.
type terms struct {
	headers       headerFunc
	authorization int64
}

// errAuthorizationChanged is a sentinel signalling that an exchange was not
// sent because the credential presented is no longer the one its terms name.
// Nothing reached the server, and the connection standing is one settled under
// the new credential.
var errAuthorizationChanged = errors.New("client: the credential presented to the server has changed")

// answered is what one exchange brought back: the result, and the connection it
// travelled over.
//
// The connection is read while the exchange is still held, which is the only
// moment it is known. A handshake settles under the same gate, so none can come
// between the response and the reading of it; a reading made once the gate is
// released may already describe the connection that replaced the one the
// response travelled over, and what is then stamped with it would pass for a
// statement of a server that never made it.
//
// receivedAt is the local time the result arrived, which is what the
// specification measures its freshness from, and authorization the
// authorization context the request was presented in.
type answered struct {
	result        json.RawMessage
	generation    int64
	receivedAt    time.Time
	authorization int64
}

// exchangeWith is dispatchWith for a caller that keeps what it read: it takes
// the terms the exchange is held to, and reports the connection the result
// travelled over along with the result.
func (p *protocol) exchangeWith(ctx context.Context, method string, params any, held terms) (answered, error) {
	return p.exchangeFor(ctx, func(ProtocolVersion) string { return method }, params, held)
}

// exchangeFor is exchangeWith for a request the revision names: methodFor is
// asked for the method of each attempt with the version of the connection that
// attempt travels over, while the exchange is held. A method chosen from a
// version read before the exchange would be a statement about whichever
// connection stood then, and a handshake settled in between may have settled
// another revision, which would be sent a request it does not define.
func (p *protocol) exchangeFor(ctx context.Context, methodFor func(ProtocolVersion) string, params any, held terms) (answered, error) {
	if err := p.acquireExchange(ctx); err != nil {
		return answered{}, err
	}
	defer p.releaseExchange()

	conn, err := p.standingConnection(ctx, held)
	if err != nil {
		return answered{}, err
	}

	method := methodFor(conn.version)
	result, err := p.attempt(ctx, method, params, conn.version, held.headers)
	if errors.Is(err, errSessionExpired) {
		conn, err = p.standingConnection(ctx, held)
		if err != nil {
			return answered{}, err
		}
		method = methodFor(conn.version)
		result, err = p.attempt(ctx, method, params, conn.version, held.headers)
	}
	if err != nil {
		return answered{}, err
	}
	receivedAt := p.now()
	// A result the server did not complete is not the answer to the call, so it
	// never reaches a decoder that would read it as an empty one.
	if unfinished := unfinishedResult(method, result); unfinished != nil {
		return answered{}, unfinished
	}
	// The gate is still held here: the deferred release runs once this value has
	// been built, so the connection read is the one the result arrived over.
	return answered{
		result:        result,
		generation:    p.connectionGeneration(),
		receivedAt:    receivedAt,
		authorization: conn.authorization,
	}, nil
}

// standingConnection settles the connection an attempt travels over and returns
// it, negotiating one when none stands or when the one that does was settled
// under another credential. It refuses, with nothing sent, an exchange whose
// terms name another authorization context than the one that connection belongs
// to. The caller holds the exchange lock.
func (p *protocol) standingConnection(ctx context.Context, held terms) (*negotiatedConnection, error) {
	p.mu.Lock()
	connecting := p.connecting
	p.mu.Unlock()
	if !connecting {
		if err := p.connectLocked(ctx); err != nil {
			return nil, err
		}
	}
	conn := p.connection()
	if conn == nil {
		return nil, newError("the client has not negotiated a protocol version")
	}
	if held.authorization != 0 && conn.authorization != held.authorization {
		return nil, errAuthorizationChanged
	}
	return conn, nil
}

// headerFunc renders the extra request headers of one exchange from the encoded
// params the attempt puts on the wire. It is called at most once per attempt,
// and only for a protocol version that carries headers.
type headerFunc func(params json.RawMessage) (map[string]string, error)

// attempt performs a single request/response exchange at the given protocol
// version, answering any server-initiated requests interleaved before the
// matching response. The caller holds the exchange lock.
func (p *protocol) attempt(ctx context.Context, method string, params any, version ProtocolVersion, extra headerFunc) (json.RawMessage, error) {
	p.useProtocol(version)

	encoded, err := p.encodeParams(params, version)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	id := jsonrpc.IntID(p.nextID)
	p.nextID++
	p.mu.Unlock()

	req := jsonrpc.Request{JSONRPC: jsonrpc.Version, ID: id, Method: method, Params: encoded}
	frame, err := json.Marshal(&req)
	if err != nil {
		return nil, wrapError(err, "unable to encode request")
	}

	var headers map[string]string
	if handshakeFor(version) == handshakeDiscovery {
		headers = mirroredHeaders(method, encoded)
		mirrored, err := renderExtraHeaders(extra, encoded)
		if err != nil {
			return nil, err
		}
		for name, value := range mirrored {
			headers[name] = value
		}
	}

	result, rpcErr, err := p.exchangeFrame(ctx, string(frame), headers, id)
	if err != nil {
		// The channel itself failed: the stream is out of step with the server,
		// so the connection goes down and the next request negotiates again.
		p.disconnectIfConnected()
		return nil, err
	}
	if rpcErr != nil {
		// The server answered on protocol terms. That is an outcome of the
		// exchange, not a failure of the channel, so the connection stands and
		// the caller may simply try something else.
		return nil, rpcErr
	}
	return result, nil
}

// renderExtraHeaders renders the extra headers of an exchange, if it has any,
// from the encoded params of the frame they travel with.
func renderExtraHeaders(extra headerFunc, params json.RawMessage) (map[string]string, error) {
	if extra == nil {
		return nil, nil
	}
	return extra(params)
}

// exchangeFrame sends one request frame and reads until the matching response
// arrives, serving any server-initiated frames in between. A JSON-RPC error
// answering the request is returned separately from a failure of the exchange:
// only the latter costs the connection.
func (p *protocol) exchangeFrame(ctx context.Context, frame string, headers map[string]string, id jsonrpc.ID) (json.RawMessage, *jsonrpc.Error, error) {
	if err := p.send(ctx, frame, headers); err != nil {
		return nil, nil, err
	}

	for {
		raw, err := p.receive(ctx)
		if err != nil {
			return nil, nil, err
		}

		if served, err := p.serveServerFrame(ctx, []byte(raw)); err != nil {
			return nil, nil, err
		} else if served {
			continue
		}

		resp, perr := jsonrpc.ParseResponse([]byte(raw))
		if perr != nil {
			return nil, nil, newError("invalid JSON-RPC response from server: " + perr.Message)
		}
		// A response for another id belongs to an exchange that has already
		// been abandoned; keep reading for ours. An error carrying a null id
		// cannot be correlated, so it answers the request in flight.
		if !bytes.Equal(resp.ID.Raw(), id.Raw()) && !(resp.ID.IsNull() && resp.Error != nil) {
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error, nil
		}
		return resp.Result, nil, nil
	}
}

// encodeParams encodes the params member of a request, adding the protocol
// metadata a discovery-era request carries. Metadata already present in the
// caller's params wins, so a caller can override any of it.
func (p *protocol) encodeParams(params any, version ProtocolVersion) (json.RawMessage, error) {
	var members map[string]json.RawMessage
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, wrapError(err, "unable to encode request params")
		}
		if handshakeFor(version) != handshakeDiscovery {
			return raw, nil
		}
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, wrapError(err, "unable to encode request params")
		}
	}
	if handshakeFor(version) != handshakeDiscovery {
		return nil, nil
	}

	if members == nil {
		members = map[string]json.RawMessage{}
	}
	meta, err := p.protocolMeta(members["_meta"], version)
	if err != nil {
		return nil, err
	}
	members["_meta"] = meta

	raw, err := json.Marshal(members)
	if err != nil {
		return nil, wrapError(err, "unable to encode request params")
	}
	return raw, nil
}

// protocolMeta builds the _meta member of a discovery-era request: the protocol
// version, the client capabilities, and the client identity, with any metadata
// the caller already supplied layered on top.
func (p *protocol) protocolMeta(existing json.RawMessage, version ProtocolVersion) (json.RawMessage, error) {
	capabilities, err := p.declaredCapabilities()
	if err != nil {
		return nil, err
	}
	meta := map[string]any{
		MetaProtocolVersion:    version,
		MetaClientCapabilities: capabilities,
		MetaClientInfo:         p.identity().ToMap(),
	}
	if len(existing) > 0 {
		var supplied map[string]any
		if err := json.Unmarshal(existing, &supplied); err != nil {
			return nil, newError("unable to encode request params: the [_meta] member must be an object")
		}
		for key, value := range supplied {
			meta[key] = value
		}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, wrapError(err, "unable to encode request params")
	}
	return raw, nil
}

// serveServerFrame handles a frame initiated by the server. It answers ping
// requests, declines other requests with method-not-found, passes the method of
// a notification on to whoever listens for them, and reports (false) for
// anything that is a client-bound response. The returned bool indicates the
// frame was consumed.
func (p *protocol) serveServerFrame(ctx context.Context, raw []byte) (bool, error) {
	var probe struct {
		Method *string         `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Method == nil {
		return false, nil
	}
	// A notification (no id) asks for no answer. What it says may still outdate
	// something the client keeps, so its method is passed on.
	if len(bytes.TrimSpace(probe.ID)) == 0 || bytes.Equal(bytes.TrimSpace(probe.ID), []byte("null")) {
		if p.notified != nil {
			p.notified(*probe.Method)
		}
		return true, nil
	}

	var id jsonrpc.ID
	_ = id.UnmarshalJSON(probe.ID)

	var resp *jsonrpc.Response
	if *probe.Method == "ping" {
		resp, _ = jsonrpc.NewResult(id, map[string]any{})
	} else {
		resp = jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeMethodNotFound,
			"method ["+*probe.Method+"] is not supported by this client")
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return true, wrapError(err, "unable to encode response to server request")
	}
	if err := p.send(ctx, string(out), nil); err != nil {
		return true, err
	}
	return true, nil
}

// notify sends a parameterless notification at the given protocol version.
func (p *protocol) notify(ctx context.Context, method string, version ProtocolVersion) error {
	p.useProtocol(version)

	n, err := jsonrpc.NewNotification(method, nil)
	if err != nil {
		return wrapError(err, "unable to encode notification")
	}
	out, err := json.Marshal(n)
	if err != nil {
		return wrapError(err, "unable to encode notification")
	}
	return p.send(ctx, string(out), nil)
}

// useProtocol tells a protocol-aware transport which version the next frames
// belong to. A transport without the hook is left alone.
func (p *protocol) useProtocol(version ProtocolVersion) {
	if aware, ok := p.transport.(ProtocolAware); ok {
		aware.UseProtocol(version)
	}
}

// send transmits a frame, handing the protocol headers to a transport that can
// carry them.
func (p *protocol) send(ctx context.Context, frame string, headers map[string]string) error {
	if sender, ok := p.transport.(HeaderSender); ok {
		return sender.SendWithHeaders(ctx, frame, headers)
	}
	return p.transport.Send(ctx, frame)
}

// receive reads the next frame.
func (p *protocol) receive(ctx context.Context) (string, error) {
	return p.transport.Receive(ctx)
}
