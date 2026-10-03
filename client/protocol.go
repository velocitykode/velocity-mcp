package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
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

	// timeout bounds one attempt at an exchange, from the request going out to
	// the response coming back, when the caller's context carries no deadline
	// of its own and the transport states no timeout (see exchangeTimeout). The
	// bound is enforced here rather than by the transport: a transport times
	// out a wait for one frame, and a server sending progress or log
	// notifications would start that clock over with every one, while the
	// specification has a maximum timeout enforced regardless of them.
	timeout time.Duration

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

	// unaccounted holds the requests this client stopped waiting for and told
	// the server so, over the channel now open, whose reply may still arrive on
	// it. A server may answer one with an error it puts no id on, and while a
	// withdrawn request is unaccounted for such an error is its reply and not
	// that of the request in flight. A server that conforms never answers a
	// cancelled request at all, so an entry does not wait for a reply for
	// ever: it lapses after withdrawnReplyWindow, the oldest goes when there
	// are more than maxUnaccounted, and all go when the channel is given up.
	unaccounted []withdrawnRequest

	// maxTimeout is the maximum one exchange may take however much progress
	// the server reports, or zero for the default of maxTimeoutFactor times the
	// timeout. nextToken numbers the progress tokens this client hands out.
	maxTimeout time.Duration
	nextToken  int64

	// silent reports that the last request over the open channel timed out and
	// the server has sent no frame of any kind since. A server that is only
	// slow on one request goes on answering others; one that says nothing at
	// all through a second timeout is taken for hung, and is given up rather
	// than cancelled at again.
	silent bool
}

// noCapabilities is the capability set of a client that declares none.
var noCapabilities = json.RawMessage(`{}`)

// newProtocol builds a protocol over the given transport and client identity.
func newProtocol(transport Transport, clientInfo schema.Implementation) *protocol {
	return &protocol{
		transport:          transport,
		now:                time.Now,
		timeout:            defaultTimeout,
		clientInfo:         clientInfo,
		clientCapabilities: noCapabilities,
		nextID:             1,
		exchange:           make(chan struct{}, 1),
	}
}

// setTimeout sets the bound on one attempt at an exchange over a transport that
// does not state its own. A duration of zero or less leaves an attempt bounded
// by the caller's context alone.
func (p *protocol) setTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.timeout = d
}

// timeoutStated is what a transport that states its timeout implements. The
// transports of this package do, so a timeout set on one, as the recipe of a
// named client sets it, bounds the exchange as well as the wait for a frame.
type timeoutStated interface {
	Timeout() time.Duration
}

// exchangeTimeout returns the bound on one attempt: the timeout the transport
// states, or the client's own over a transport that states none.
func (p *protocol) exchangeTimeout() time.Duration {
	if stated, ok := p.transport.(timeoutStated); ok {
		return stated.Timeout()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.timeout
}

// maxTimeoutFactor is how many times the timeout an exchange may take at the
// most, when no maximum was set: long enough for a call that reports progress
// to do real work, and short enough that a caller is never held for good.
const maxTimeoutFactor = 10

// setMaxTimeout sets the maximum an exchange may take whatever progress the
// server reports. A duration of zero or less restores the default.
func (p *protocol) setMaxTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		d = 0
	}
	p.maxTimeout = d
}

// exchangeMaximum returns the maximum an exchange may take, given its timeout.
func (p *protocol) exchangeMaximum(timeout time.Duration) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.maxTimeout > 0 {
		return p.maxTimeout
	}
	return timeout * maxTimeoutFactor
}

// bounded returns the context one attempt runs under. A caller's own deadline
// is the bound when the context carries one, and nothing here cuts the attempt
// before it. Otherwise the attempt runs under a clock: the timeout, which
// progress reported for the request starts over, inside the maximum, which
// nothing does. The clock is nil when the caller's deadline is the bound.
//
// The cancel function releases whatever the attempt still holds, as the response
// stream a transport left open, and is called once the attempt is over.
func (p *protocol) bounded(ctx context.Context) (context.Context, *exchangeClock, context.CancelFunc) {
	timeout := p.exchangeTimeout()
	if _, hasDeadline := ctx.Deadline(); hasDeadline || timeout <= 0 {
		held, cancel := context.WithCancel(ctx)
		return held, nil, cancel
	}
	capped, cancel := context.WithTimeout(ctx, p.exchangeMaximum(timeout))
	clock := newExchangeClock(capped, timeout)
	return clock, clock, func() {
		clock.release()
		cancel()
	}
}

// boundedOnce returns a context for work made of several requests that is given
// the time of one as a whole: the caller's own when it carries a deadline, and
// otherwise the caller's bounded by the timeout.
func (p *protocol) boundedOnce(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := p.exchangeTimeout()
	if _, hasDeadline := ctx.Deadline(); hasDeadline || timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
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
	encoded, err := marshalWire(capabilities)
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
	p.unaccounted = nil
	p.silent = false
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
func (p *protocol) dispatchWith(ctx context.Context, method string, params any, extra mirrorFunc) (json.RawMessage, error) {
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
	headers       mirrorFunc
	authorization int64
}

// mirrorFunc renders the extra request headers of one attempt from the encoded
// params the attempt puts on the wire. It is called at most once per attempt,
// only for a protocol version that carries headers, and while the exchange is
// held: under is the connection the attempt travels over, which nothing can
// replace until the attempt is done, and over which the function may ask the
// server what it needs to know before the frame goes out.
type mirrorFunc func(ctx context.Context, under heldConnection, params json.RawMessage) (map[string]string, error)

// heldConnection is the connection an exchange travels over, in the hands of
// whoever holds that exchange. The gate is held for as long as the value is in
// use, so the connection it names is the one standing, the credential its
// frames present is the one that was settled for the exchange, and a request
// made through it travels over the same connection as the request it serves.
type heldConnection struct {
	proto         *protocol
	version       ProtocolVersion
	generation    int64
	authorization int64
}

// ask makes a request over the held connection and reports what it brought
// back, as an exchange of its own would.
func (h heldConnection) ask(ctx context.Context, method string, params any) (answered, error) {
	result, err := h.proto.attempt(ctx, method, params, h.version, nil)
	if err != nil {
		return answered{}, err
	}
	return answered{
		result:        result,
		generation:    h.generation,
		receivedAt:    h.proto.now(),
		authorization: h.authorization,
	}, nil
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
	result, err := p.attempt(ctx, method, params, conn.version, p.rendering(ctx, held, conn))
	if errors.Is(err, errSessionExpired) {
		conn, err = p.standingConnection(ctx, held)
		if err != nil {
			return answered{}, err
		}
		method = methodFor(conn.version)
		result, err = p.attempt(ctx, method, params, conn.version, p.rendering(ctx, held, conn))
	}
	if isUnsupportedVersion(err) {
		conn, err = p.renegotiated(ctx, conn, err)
		if err != nil {
			return answered{}, err
		}
		method = methodFor(conn.version)
		result, err = p.attempt(ctx, method, params, conn.version, p.rendering(ctx, held, conn))
	}
	if err != nil {
		return answered{}, err
	}
	// The gate is still held here: the deferred release runs once this value has
	// been built, so the connection read is the one the result arrived over.
	return answered{
		result:        result,
		generation:    p.connectionGeneration(),
		receivedAt:    p.now(),
		authorization: conn.authorization,
	}, nil
}

// isUnsupportedVersion reports whether a failure is the server refusing the
// protocol version a request was made at.
func isUnsupportedVersion(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == CodeUnsupportedProtocolVersion
}

// renegotiated settles the connection again after the server refused a request
// for the version the connection was settled on, and returns the connection to
// repeat the request over. The caller holds the exchange lock.
//
// A version is agreed once, by the handshake, and a server may stop accepting
// it afterwards: it was rolled back, or the request reached another replica.
// The specification has a client that is told so select a version both sides
// support and try again, and probe again when what it assumed of the server no
// longer holds. The refused request did nothing on the server, so repeating it
// is safe. The handshake runs over the channel that is open, as it does when
// what a server advertised has run out, and the connection it settles is a new
// one to everything that dates what it keeps by the connection it was read
// over.
//
// The rejection stands when there is nothing to renegotiate: the version is
// pinned, so no other may be settled, or the handshake settles the very version
// the server just refused, and a second attempt at it would be refused again.
func (p *protocol) renegotiated(ctx context.Context, refused *negotiatedConnection, rejection error) (*negotiatedConnection, error) {
	if pinned, _ := p.pinnedVersion(); pinned != "" {
		return nil, rejection
	}
	p.mu.Lock()
	p.connected = false
	p.conn = nil
	p.mu.Unlock()
	if err := p.settle(ctx, refused.authorization); err != nil {
		return nil, err
	}
	conn := p.connection()
	if conn.version == refused.version {
		return nil, rejection
	}
	return conn, nil
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

// headerFunc renders the extra request headers of one attempt from the encoded
// params it puts on the wire: the mirrorFunc of an exchange, bound to the
// connection that attempt travels over.
type headerFunc func(params json.RawMessage) (map[string]string, error)

// rendering binds the extra headers an exchange is held to render to the
// connection one attempt at it travels over. The caller holds the exchange
// lock.
func (p *protocol) rendering(ctx context.Context, held terms, conn *negotiatedConnection) headerFunc {
	if held.headers == nil {
		return nil
	}
	under := heldConnection{
		proto:         p,
		version:       conn.version,
		generation:    p.connectionGeneration(),
		authorization: conn.authorization,
	}
	return func(params json.RawMessage) (map[string]string, error) {
		return held.headers(ctx, under, params)
	}
}

// attempt performs a single request/response exchange at the given protocol
// version, answering any server-initiated requests interleaved before the
// matching response. The caller holds the exchange lock.
//
// Every result passes through here, the handshake's included, and none is
// returned unless it is the whole answer: a result the server did not complete,
// one stating a result type the specification does not define, or one that is
// not an object at all is reported instead, so no decoder ever reads it as an
// empty success.
func (p *protocol) attempt(ctx context.Context, method string, params any, version ProtocolVersion, extra headerFunc) (json.RawMessage, error) {
	p.useProtocol(version)

	// A request that asks for progress carries a progress token of the
	// client's own unless the caller's params already name one, which is what
	// lets a server say it is still working and keeps the timeout from cutting
	// work that is under way (see exchangeClock).
	var token json.RawMessage
	if p.asksForProgress(method) {
		p.mu.Lock()
		p.nextToken++
		token = json.RawMessage(strconv.FormatInt(p.nextToken, 10))
		p.mu.Unlock()
	}
	encoded, err := p.encodeParams(params, version, token)
	if err != nil {
		return nil, err
	}
	token = progressTokenOf(encoded)

	// The headers are rendered before the request is given its id and its
	// bound: rendering them may ask the server something first, and what that
	// takes is not time the request itself has had.
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

	p.mu.Lock()
	id := jsonrpc.IntID(p.nextID)
	p.nextID++
	p.mu.Unlock()

	req := jsonrpc.Request{JSONRPC: jsonrpc.Version, ID: id, Method: method, Params: encoded}
	frame, err := marshalWire(&req)
	if err != nil {
		return nil, wrapError(err, "unable to encode request")
	}

	ctx, clock, cancel := p.bounded(ctx)
	defer cancel()

	result, rpcErr, err := p.exchangeFrame(ctx, method, string(frame), headers, id, version, progressOf{clock: clock, token: token})
	if err != nil {
		// A request the client stopped waiting for is withdrawn where the
		// transport lets the connection outlive it. Anything else is the
		// channel itself failing: the stream is out of step with the server, so
		// the connection goes down and the next request negotiates again.
		if !p.withdrawn(ctx, err, id, version) {
			p.disconnectIfConnected()
		}
		return nil, err
	}
	if rpcErr != nil {
		// The server answered on protocol terms. That is an outcome of the
		// exchange, not a failure of the channel, so the connection stands and
		// the caller may simply try something else.
		return nil, rpcErr
	}
	if unfinished := unfinishedResult(method, result); unfinished != nil {
		return nil, unfinished
	}
	return result, nil
}

// cancellationGrace bounds the sending of a cancellation. It is sent on behalf
// of a request whose own time is up, so it is given a moment of its own, and a
// short one: the exchange gate is held while it goes out.
const cancellationGrace = time.Second

// withdrawn reports whether a failed exchange was a request the client stopped
// waiting for and has withdrawn, so that the connection stands. The MCP
// specification (basic/utilities/cancellation) has the client cancel such a
// request, a timeout the same way as a caller giving up, and over stdio it has
// to: the server is one process for every request, not one per call.
//
// Only a transport that says an abandoned request leaves its channel in use
// takes part (see CancellationAware), and only once the connection is settled:
// a handshake request is not one a client may cancel, and what a failed
// handshake costs is the negotiation's to decide. A cancellation that cannot be
// sent is the channel failing after all.
func (p *protocol) withdrawn(ctx context.Context, failure error, id jsonrpc.ID, version ProtocolVersion) bool {
	aware, ok := p.transport.(CancellationAware)
	if !ok {
		return false
	}
	var timeoutErr *TimeoutError
	timedOut := errors.As(failure, &timeoutErr) || errors.Is(failure, context.DeadlineExceeded)
	if !timedOut && !errors.Is(failure, context.Canceled) {
		return false
	}
	p.mu.Lock()
	settled := p.connected && !p.connecting
	// A second timeout in a row with nothing heard from the server in between
	// is not a slow request, it is a server that has stopped: keeping the
	// connection would have every request after this one wait out its timeout
	// against the same silence. The connection is given up, so the next request
	// starts over, with a new subprocess where the server is one.
	hung := timedOut && p.silent
	if settled && timedOut {
		p.silent = !hung
	}
	p.mu.Unlock()
	if !settled || hung {
		return false
	}
	if !aware.NotifiesCancellation(version) {
		return true
	}

	reason := "the caller withdrew the request"
	if timedOut {
		reason = "the request timed out"
	}
	// The context of the exchange is done, which is why the request is being
	// withdrawn; the cancellation travels under one of its own.
	notice, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancellationGrace)
	defer cancel()
	params := map[string]any{"requestId": id.Raw(), "reason": reason}
	if p.notify(notice, "notifications/cancelled", params, version) != nil {
		return false
	}
	p.account(id)
	return true
}

// withdrawnRequest is a request this client withdrew, and when.
type withdrawnRequest struct {
	id string
	at time.Time
}

// withdrawnReplyWindow is how long after a request was withdrawn an error
// carrying a null id is still taken for its reply. A server that answers a
// cancelled request does so when the cancellation reaches it; past that, an
// error nobody can correlate belongs to the request in flight, as it always
// did.
const withdrawnReplyWindow = 5 * time.Second

// maxUnaccounted caps how many withdrawn requests are kept at once.
const maxUnaccounted = 64

// repliesPerRequest is implemented by a transport that carries the reply to
// each request on a channel of that request's own, as streamable HTTP carries
// it on the response to the POST: nothing a server sends late can reach a later
// request there, so nothing is kept to tell the two apart.
type repliesPerRequest interface {
	repliesPerRequest()
}

// account records a request withdrawn over a channel on which its reply may
// still arrive.
func (p *protocol) account(id jsonrpc.ID) {
	if _, apart := p.transport.(repliesPerRequest); apart {
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lapseLocked(now)
	p.unaccounted = append(p.unaccounted, withdrawnRequest{id: string(id.Raw()), at: now})
	if excess := len(p.unaccounted) - maxUnaccounted; excess > 0 {
		p.unaccounted = append(p.unaccounted[:0], p.unaccounted[excess:]...)
	}
}

// lapseLocked drops the withdrawn requests whose reply is no longer waited
// for. The caller holds p.mu.
func (p *protocol) lapseLocked(now time.Time) {
	kept := p.unaccounted[:0]
	for _, withdrawn := range p.unaccounted {
		if now.Sub(withdrawn.at) < withdrawnReplyWindow {
			kept = append(kept, withdrawn)
		}
	}
	p.unaccounted = kept
}

// heard records that the server sent a frame, whatever it was: it is not silent.
func (p *protocol) heard() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.silent = false
}

// accountedFor records that the reply to a withdrawn request has arrived.
func (p *protocol) accountedFor(id jsonrpc.ID) {
	raw := string(id.Raw())
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, withdrawn := range p.unaccounted {
		if withdrawn.id == raw {
			p.unaccounted = append(p.unaccounted[:index], p.unaccounted[index+1:]...)
			return
		}
	}
}

// repliesToWithdrawn reports whether an error carrying a null id is the reply to
// a request this client withdrew, and accounts for one such request when it is.
// The error cannot say which request it answers, so it is taken for the oldest
// business still open on the channel: a request withdrawn within the window in
// which a server answers one. Each withdrawn request takes one such error with
// it and no more, and when none is open the error answers the request in
// flight.
func (p *protocol) repliesToWithdrawn() bool {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lapseLocked(now)
	if len(p.unaccounted) == 0 {
		return false
	}
	p.unaccounted = append(p.unaccounted[:0], p.unaccounted[1:]...)
	return true
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
// arrives, serving any server-initiated frames in between as the revision of the
// connection allows. A JSON-RPC error answering the request is returned
// separately from a failure of the exchange: only the latter costs the
// connection.
//
// The context is consulted after every frame that was not the response, so the
// bound on the exchange holds over any transport: one that honours the context
// ends the read itself, and reports the end in its own words, and one that does
// not is read from no more once the context is done, however many frames it
// would still deliver.
//
// A progress notification naming the token of this request starts its timeout
// over; any other frame leaves the clock running.
func (p *protocol) exchangeFrame(ctx context.Context, method, frame string, headers map[string]string, id jsonrpc.ID, version ProtocolVersion, progress progressOf) (json.RawMessage, *jsonrpc.Error, error) {
	if err := p.send(ctx, frame, headers); err != nil {
		return nil, nil, err
	}

	for passedOver := false; ; passedOver = true {
		if err := ctx.Err(); passedOver && err != nil {
			return nil, nil, exchangeEnded(method, err)
		}
		raw, err := p.receive(ctx)
		if err != nil {
			return nil, nil, err
		}
		p.heard()
		progress.report([]byte(raw))

		if served, err := p.serveServerFrame(ctx, []byte(raw), version); err != nil {
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
		// cannot be correlated: it answers the request in flight, unless a
		// request withdrawn before this one was sent is still unanswered, in
		// which case it is that request's reply and is passed over with it.
		switch {
		case bytes.Equal(resp.ID.Raw(), id.Raw()):
		case !resp.ID.IsNull():
			p.accountedFor(resp.ID)
			continue
		case resp.Error == nil || p.repliesToWithdrawn():
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error, nil
		}
		return resp.Result, nil, nil
	}
}

// progressDeaf is implemented by a transport on which reported progress cannot
// keep a request alive, because the transport bounds the whole request by a
// timeout of its own. A request over it asks for no progress: the token would
// only have a server report progress that changes nothing. It is the one place
// that decides this for a transport: a transport that learns to let progress
// extend a request stops implementing it, and its requests ask.
type progressDeaf interface {
	progressDeaf()
}

// asksForProgress reports whether a request of the given method carries a
// progress token of the client's own. The requests that do are the ones a
// server may take long over, because they run something of the server's or of
// what it fronts: calling a tool, reading a resource, rendering a prompt. A
// listing, a liveness check, and the handshake do not, and are sent as they
// always were.
func (p *protocol) asksForProgress(method string) bool {
	if _, deaf := p.transport.(progressDeaf); deaf {
		return false
	}
	switch method {
	case "tools/call", "resources/read", "prompts/get":
		return true
	default:
		return false
	}
}

// progressOf is what one exchange listens for progress with: the clock its
// timeout runs on, which is nil when the caller's deadline is the bound, and
// the progress token its request carried, which is nil when it carried none.
type progressOf struct {
	clock *exchangeClock
	token json.RawMessage
}

// report starts the timeout over when the frame is a progress notification for
// this exchange's token. A notification of anything else, or of progress on
// another request, is not this request being worked on.
func (o progressOf) report(frame []byte) {
	if o.clock == nil || o.token == nil {
		return
	}
	var notification struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Token json.RawMessage `json:"progressToken"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &notification); err != nil || len(notification.ID) != 0 ||
		notification.Method != "notifications/progress" {
		return
	}
	if bytes.Equal(trimJSONSpace(notification.Params.Token), o.token) {
		o.clock.progressed()
	}
}

// progressTokenOf returns the progress token the encoded params of a request
// carry, or nil when they carry none.
func progressTokenOf(encoded json.RawMessage) json.RawMessage {
	var params struct {
		Meta struct {
			Token json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(encoded, &params); err != nil {
		return nil
	}
	token := trimJSONSpace(params.Meta.Token)
	if len(token) == 0 || bytes.Equal(token, []byte("null")) {
		return nil
	}
	return token
}

// exchangeEnded reports an exchange its context ended before the response
// arrived, while the transport went on delivering other frames. A deadline that
// passed is a timeout like any other; a cancelled context is the caller
// withdrawing the request, which is neither a timeout nor a failure of the
// channel.
func exchangeEnded(method string, cause error) error {
	if errors.Is(cause, context.Canceled) {
		return wrapError(cause, "the wait for a response to ["+method+"] was cancelled")
	}
	return NewTimeoutError("timed out while waiting for a response to ["+method+"]", cause)
}

// marshalWire encodes a value for the wire. It is json.Marshal without the
// escaping of the characters HTML gives a meaning to, which the standard
// encoder applies to everything it writes, a json.RawMessage included: a member
// the client holds raw in order to send it back exactly as it arrived, as the
// state token of an unfinished result and the cursor of a listing are, would
// leave with its ampersands, angle brackets and line separators rewritten as
// escapes. Every frame the client sends is encoded here, so what was kept byte
// for byte goes out byte for byte.
func marshalWire(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	// The encoder ends what it writes with a newline, which is no part of it.
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// encodeParams encodes the params member of a request, adding the protocol
// metadata a discovery-era request carries and, when token is set, the progress
// token the request asks for progress under. Metadata already present in the
// caller's params wins, so a caller can override any of it.
func (p *protocol) encodeParams(params any, version ProtocolVersion, token json.RawMessage) (json.RawMessage, error) {
	discovery := handshakeFor(version) == handshakeDiscovery
	var members map[string]json.RawMessage
	if params != nil {
		raw, err := marshalWire(params)
		if err != nil {
			return nil, wrapError(err, "unable to encode request params")
		}
		if !discovery && token == nil {
			return raw, nil
		}
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, wrapError(err, "unable to encode request params")
		}
	}
	if !discovery && token == nil {
		return nil, nil
	}

	if members == nil {
		members = map[string]json.RawMessage{}
	}
	meta, err := p.protocolMeta(members["_meta"], version, token)
	if err != nil {
		return nil, err
	}
	members["_meta"] = meta

	raw, err := marshalWire(members)
	if err != nil {
		return nil, wrapError(err, "unable to encode request params")
	}
	return raw, nil
}

// protocolMeta builds the _meta member of a request: on the discovery revision
// the protocol version, the client capabilities, and the client identity, and
// on any revision the progress token when one is given, with any metadata the
// caller already supplied layered on top.
func (p *protocol) protocolMeta(existing json.RawMessage, version ProtocolVersion, token json.RawMessage) (json.RawMessage, error) {
	meta := map[string]any{}
	if handshakeFor(version) == handshakeDiscovery {
		capabilities, err := p.declaredCapabilities()
		if err != nil {
			return nil, err
		}
		meta[MetaProtocolVersion] = version
		meta[MetaClientCapabilities] = capabilities
		meta[MetaClientInfo] = p.identity().ToMap()
	}
	if token != nil {
		meta["progressToken"] = token
	}
	if len(existing) > 0 {
		var supplied map[string]json.RawMessage
		if err := json.Unmarshal(existing, &supplied); err != nil || supplied == nil {
			return nil, newError("unable to encode request params: the [_meta] member must be an object")
		}
		for key, value := range supplied {
			meta[key] = value
		}
	}
	raw, err := marshalWire(meta)
	if err != nil {
		return nil, wrapError(err, "unable to encode request params")
	}
	return raw, nil
}

// serveServerFrame handles a frame initiated by the server, as the revision of
// the connection it arrived over allows. It passes the method of a notification
// on to whoever listens for them, and reports (false) for anything that is a
// client-bound response. The returned bool indicates the frame was consumed.
//
// A request from the server is answered only over the initialize-era revisions,
// which let either peer send one: ping is answered and anything else declined
// with method-not-found, over whichever transport carries the connection. The
// discovery revision forbids a server to send requests and a client to send
// responses on both of its transport bindings, so a request arriving over such
// a connection is the server breaking the protocol, and the exchange fails with
// nothing written back: a client that answered would break it too, and a server
// could have it write one frame for every frame it sent.
func (p *protocol) serveServerFrame(ctx context.Context, raw []byte, version ProtocolVersion) (bool, error) {
	var probe struct {
		Method *string         `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Method == nil {
		return false, nil
	}
	// A notification has no id member at all and asks for no answer. What it
	// says may still outdate something the client keeps, so its method is
	// passed on.
	if len(probe.ID) == 0 {
		if p.notified != nil {
			p.notified(*probe.Method)
		}
		return true, nil
	}
	// The specification gives a request an id that is a string or a number,
	// never null, and a notification none. A frame naming a method under any
	// other id is neither: it is not acted on as a notification, which would
	// let a frame the protocol does not define outdate what the client keeps,
	// and there is no id to answer it under. It ends the exchange as any other
	// frame the client cannot read does, on every revision.
	var id jsonrpc.ID
	if err := id.UnmarshalJSON(probe.ID); err != nil || !id.IsValidRequestID() {
		return true, newError("invalid JSON-RPC message from server: the [" + *probe.Method +
			"] frame carries an id that is neither a string nor a number")
	}
	if handshakeFor(version) == handshakeDiscovery {
		return true, newError("the server sent a [" + *probe.Method + "] request over a connection of protocol version [" +
			version + "], which forbids it; this client sends no response")
	}

	var resp *jsonrpc.Response
	if *probe.Method == "ping" {
		resp, _ = jsonrpc.NewResult(id, map[string]any{})
	} else {
		resp = jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeMethodNotFound,
			"method ["+*probe.Method+"] is not supported by this client")
	}
	out, err := marshalWire(resp)
	if err != nil {
		return true, wrapError(err, "unable to encode response to server request")
	}
	if err := p.send(ctx, string(out), nil); err != nil {
		return true, err
	}
	return true, nil
}

// notify sends a notification at the given protocol version. Its params are
// encoded as those of a request are, so a discovery-era notification carries the
// protocol metadata and the headers mirroring it, and one of an initialize-era
// revision carries what it was given and nothing else.
func (p *protocol) notify(ctx context.Context, method string, params any, version ProtocolVersion) error {
	p.useProtocol(version)

	encoded, err := p.encodeParams(params, version, nil)
	if err != nil {
		return err
	}
	out, err := marshalWire(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: jsonrpc.Version, Method: method, Params: encoded})
	if err != nil {
		return wrapError(err, "unable to encode notification")
	}
	var headers map[string]string
	if handshakeFor(version) == handshakeDiscovery {
		headers = mirroredHeaders(method, encoded)
	}
	return p.send(ctx, string(out), headers)
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
