package client

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// Client is an MCP client bound to a single transport. It connects lazily: the
// first request that needs a connection performs the initialize handshake. A
// Client is safe for sequential use; the underlying protocol serializes the
// request/response exchange.
type Client struct {
	proto      *protocol
	transport  Transport
	clientInfo schema.Implementation
	name       string

	// mu guards the record of the tools the last listing refused and the
	// mirrored parameters read from the definitions it accepted. catalogued
	// reports whether a listing has read the whole catalogue, which is what
	// makes a name it does not carry a name the server does not advertise, and
	// cataloguedUntil the moment that stops being so: the first of its pages to
	// go stale takes the claim with it. listedOn is the connection those
	// definitions were read over: they describe the server that stated them and
	// nothing beyond it.
	//
	// changes counts the times the server has said its catalogue changed. A
	// definition belongs to the count it was read at: the specification has
	// that notification outdate a listing at once, however much of its lifetime
	// is left.
	//
	// excludedFor is the authorization context the listing that refused those
	// tools was asked for in. Which tools a server advertises may depend on who
	// is asking, so the names it refused are shown in that context alone. The
	// definitions need no such mark: a change of credential is a change of
	// connection, and they are dropped with the connection they were read over.
	mu              sync.Mutex
	excluded        []ExcludedTool
	excludedFor     int64
	mirrored        map[string]statedDefinition
	catalogued      bool
	cataloguedUntil time.Time
	listedOn        int64
	changes         int64
}

// toolsChangedNotification is the notification a server sends when the list of
// tools it advertises has changed.
const toolsChangedNotification = "notifications/tools/list_changed"

// statedDefinition is what one listing read of a tool: the parameters its
// definition asks to be mirrored, the moment the page that stated them stops
// being fresh, and the count of changes the server had announced to its
// catalogue when the listing set out. The server gave the page that lifetime,
// and past it, or past the next change, the definition is what the server said
// once rather than what it says.
type statedDefinition struct {
	params     mirroredParameters
	staleAfter time.Time
	changes    int64
}

// crossedGeneration stands for the connection a listing belongs to when it
// belongs to none: its pages were read over more than one of them. No handshake
// ever settles it, so a definition stamped with it is stale against every
// connection, which is what it is.
const crossedGeneration int64 = -1

// defaultClientInfo identifies this client to servers when no clientInfo is set.
func defaultClientInfo() schema.Implementation {
	return schema.NewImplementation("Velocity MCP Client", "0.1.0")
}

// New builds a Client over an arbitrary transport with the given client identity.
// A zero clientInfo (empty name) falls back to a default identity.
func New(transport Transport, clientInfo schema.Implementation) *Client {
	if clientInfo.Name == "" {
		clientInfo = defaultClientInfo()
	}
	c := &Client{
		proto:      newProtocol(transport, clientInfo),
		transport:  transport,
		clientInfo: clientInfo,
	}
	c.proto.notified = c.serverNotified
	return c
}

// serverNotified weighs a notification the server sent. One saying the list of
// tools has changed outdates every definition read before it: the specification
// makes it an immediate invalidation of a listing that is still fresh, so the
// record is dropped and the definitions callers still hold are read again when
// they are next called.
func (c *Client) serverNotified(method string) {
	if method != toolsChangedNotification {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mirrored = nil
	c.catalogued = false
	c.cataloguedUntil = time.Time{}
	c.changes++
}

// catalogueChanges returns the count of changes the server has announced to its
// catalogue, which dates a definition read now.
func (c *Client) catalogueChanges() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changes
}

// Local builds a Client that talks to a server subprocess over stdio.
func Local(command string, args ...string) *Client {
	return New(NewStdioTransport(command, args...), schema.Implementation{})
}

// Web builds a WebClient that talks to a server over streamable HTTP, adding
// bearer-token and OAuth helpers.
func Web(url string) *WebClient {
	transport := NewHTTPTransport(url)
	return &WebClient{
		Client:    New(transport, schema.Implementation{}),
		transport: transport,
	}
}

// WithClientInfo overrides the client identity. It takes effect on the next
// handshake.
func (c *Client) WithClientInfo(info schema.Implementation) *Client {
	if info.Name == "" {
		info = defaultClientInfo()
	}
	c.clientInfo = info
	c.proto.setClientInfo(info)
	return c
}

// WithClientCapabilities declares what this client can do for a server: the
// capability object the protocol metadata of every request carries. A server
// asks for the inputs of an unfinished result only for a capability the client
// declared, so elicitation, sampling, and roots are reachable only through
// this. A nil or empty map declares none, which is the default.
//
// The declaration belongs to the revision that asks for those inputs as part of
// a result, and travels on a connection settled that way alone. A connection
// settled by the initialize handshake declares none whatever is set here: that
// revision asks by sending the client a request of its own, which this client
// does not serve, and declaring a capability there would invite the very
// requests it would have to refuse.
//
// It takes effect on the next request. A set that cannot be encoded as JSON is
// reported by that request rather than here, so the call still chains.
func (c *Client) WithClientCapabilities(capabilities map[string]any) *Client {
	c.proto.setClientCapabilities(capabilities)
	return c
}

// WithProtocolVersion pins the protocol version the client offers, skipping the
// negotiation probe: the pinned version is offered with the handshake it
// belongs to and a server that settles on another version is refused. An empty
// version clears the pin and restores negotiation. Pinning after connecting
// drops the connection, so the next request negotiates again.
//
// A version this client does not speak is reported by the next Connect or
// request rather than here, so the call still chains.
func (c *Client) WithProtocolVersion(version ProtocolVersion) *Client {
	c.proto.pinProtocolVersion(version)
	return c
}

// ProtocolVersion returns the negotiated protocol version, connecting first
// when the client has not negotiated one yet. The version is that of the
// connection this call found standing or settled, read before anything else
// could replace it.
func (c *Client) ProtocolVersion(ctx context.Context) (ProtocolVersion, error) {
	found, err := c.proto.connect(ctx)
	if err != nil {
		return "", err
	}
	return found.version, nil
}

// Capabilities returns the capabilities the server advertises, connecting first
// when needed. What a discover result advertised is kept for as long as the
// server said it may be and asked for again after that, so the answer is never
// older than the lifetime the server gave it.
func (c *Client) Capabilities(ctx context.Context) (map[string]any, error) {
	conn, err := c.proto.advertised(ctx)
	if err != nil {
		return nil, err
	}
	return conn.capabilities(), nil
}

// ServerInfo returns the identity the server advertises, connecting first when
// needed and asking again once what it advertised has run out, as Capabilities
// does. Its Name is empty when the server advertised none.
func (c *Client) ServerInfo(ctx context.Context) (schema.Implementation, error) {
	conn, err := c.proto.advertised(ctx)
	if err != nil {
		return schema.Implementation{}, err
	}
	return conn.serverInfo(), nil
}

// Instructions returns the usage guidance the server advertises, connecting
// first when needed and asking again once what it advertised has run out, as
// Capabilities does.
func (c *Client) Instructions(ctx context.Context) (string, error) {
	conn, err := c.proto.advertised(ctx)
	if err != nil {
		return "", err
	}
	return conn.instructions(), nil
}

// DiscoverResult returns the discover result the connection was settled with,
// or nil when the connection was negotiated through the initialize handshake
// (or not at all). It is the record of that handshake and as old as the
// handshake is: it sends nothing, so it cannot ask again. Capabilities,
// ServerInfo, and Instructions are what to ask for what the server advertises
// now.
//
// The record belongs to the credential the handshake presented. Once the
// transport would present another one it is nil, as it is before any
// connection: what a server told one caller is not shown to the next.
func (c *Client) DiscoverResult() *DiscoverResult { return c.proto.discoverResult() }

// WithTimeout sets the per-operation timeout on the transport.
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.transport.SetTimeout(d)
	return c
}

// ClientInfo returns the client identity sent on initialize.
func (c *Client) ClientInfo() schema.Implementation { return c.clientInfo }

// Connect performs the initialize handshake. It is optional: any request
// connects on demand.
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.proto.connect(ctx)
	return err
}

// Disconnect tears down the transport.
func (c *Client) Disconnect() { c.proto.disconnect() }

// Connected reports whether the initialize handshake has completed.
func (c *Client) Connected() bool { return c.proto.isConnected() }

// InitializeResult returns the server's initialize result, or nil before
// connect. Like DiscoverResult, it is nil once the transport would present
// another credential than the one the handshake did.
func (c *Client) InitializeResult() *InitializeResult { return c.proto.initializeResult() }

// Ping asks the server whether it is still answering, connecting first when
// needed. The request it sends is the one the negotiated revision defines for
// that: the ping request over the initialize-era revisions, and server/discover
// over the revision that removed ping, which requires every server to serve
// server/discover. The answer is discarded either way, since what is being
// checked is that one arrives at all.
//
// The request is named by the exchange that sends it, for the connection it
// travels over. The revision is not asked for ahead of it: a handshake settled
// in between, under another credential or against a server that has been
// replaced, may settle another revision, and a server asked for the request of
// the one before would be reported as not answering for being alive.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.proto.exchangeFor(ctx, livenessMethod, nil, terms{})
	return err
}

// Tools lists the server's tools, following pagination. An optional limit caps
// the number returned.
//
// A tool whose schema annotates its input properties for header mirroring in a
// way the specification forbids is left out: its definition is invalid, and
// advertising it would offer a call this client could only make wrongly. One
// such tool must not cost the rest of the catalogue, so the others are returned
// as usual and the reasons are available from ExcludedTools.
func (c *Client) Tools(ctx context.Context, limit ...int) ([]Tool, error) {
	tools, _, err := c.listTools(ctx, limit)
	return tools, err
}

// listTools reads the catalogue and reports the connection the definitions it
// returns were read over, so a call weighing itself against one of them knows
// which server stated it.
//
// A listing whose pages did not all travel over the same connection is the
// catalogue of no server, and is stamped as belonging to none. Its entries are
// still returned, because they are what the server answered with and a caller
// reading the catalogue has nothing better; what they may not do is settle the
// headers of a later call without being read again.
func (c *Client) listTools(ctx context.Context, limit []int) ([]Tool, int64, error) {
	// The count is read before the first page is asked for, so a change the
	// server announces while the listing is being read outdates it as well:
	// nothing says which of its pages the server had already put together.
	changes := c.catalogueChanges()
	read, err := c.listOn(ctx, "tools", limit)
	if err != nil {
		return nil, crossedGeneration, err
	}
	tools := make([]Tool, 0, len(read.entries))
	var excluded []ExcludedTool
	mirrored := make(map[string]statedDefinition, len(read.entries))
	for _, e := range read.entries {
		t, err := parseTool(c, e.payload)
		if err != nil {
			return nil, crossedGeneration, err
		}
		if t.mirrorErr != nil {
			excluded = append(excluded, ExcludedTool{Name: t.Name, Err: t.mirrorErr})
			continue
		}
		t.staleAfter = e.staleAfter
		t.changes = changes
		mirrored[t.Name] = statedDefinition{params: t.mirrored, staleAfter: e.staleAfter}
		tools = append(tools, t)
	}
	generation := c.recordListing(excluded, mirrored, len(limit) == 0, read, changes)
	for index := range tools {
		tools[index].generation = generation
	}
	return tools, generation, nil
}

// ExcludedTool is a tool the server advertised that this client refuses to use,
// with the reason it was refused.
type ExcludedTool struct {
	Name string
	Err  error
}

// ExcludedTools returns the tools the most recent Tools call left out of the
// catalogue, so a caller can report why a tool it expected is missing. It
// returns none once the credential presented to the server is no longer the one
// that listing was asked for with: the catalogue was the server's answer to
// another caller, and its names are not this one's to be shown.
func (c *Client) ExcludedTools() []ExcludedTool {
	authorization := c.proto.authorizationNow()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.excludedFor != authorization {
		return nil
	}
	return append([]ExcludedTool(nil), c.excluded...)
}

// recordListing records what one listing found: the tools it refused, and the
// mirrored parameters of the definitions it accepted. A whole listing is the
// catalogue as it now stands and replaces what was known, so a tool the server
// has dropped, or whose refreshed definition this client now refuses, leaves no
// stale parameters behind for a later call to mirror. A capped listing is only
// a part of the catalogue, so what it did carry is merged into the rest; a tool
// it refused is dropped there too, because the definition this client last read
// of it is one it will not mirror from.
//
// The record belongs to the connection the listing was read over, which the
// listing states: the one every page travelled over, or crossedGeneration when
// they did not all travel over one. A listing that crossed a handshake read its
// pages from more than one server, so it records nothing and reports that it
// belongs to no connection. What an earlier listing settled over the connection
// now standing is left as it was, being the one thing here that describes it.
//
// Every definition is recorded with the moment the page that stated it stops
// being fresh, and a whole listing with the moment the first of its pages does:
// the record is what the server said, for as long as the server said it may be
// kept. A listing the server outdated while it was being read, by announcing
// that its catalogue had changed, records nothing either: changes is the count
// of such announcements the listing set out at.
//
// It returns the connection the listing belongs to: the one now standing when
// that is the one it was read over, and crossedGeneration when it is not.
func (c *Client) recordListing(excluded []ExcludedTool, mirrored map[string]statedDefinition, whole bool, read listing, changes int64) int64 {
	generation := c.proto.connectionGeneration()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetOtherConnection(generation)
	c.excluded, c.excludedFor = excluded, read.authorization
	if read.readOn != generation {
		return crossedGeneration
	}
	if changes != c.changes {
		// The listing did travel over this connection, so that is what it
		// reports; what dates its definitions is the count they set out at.
		return generation
	}
	if whole {
		c.mirrored = mirrored
		c.catalogued = true
		c.cataloguedUntil = read.staleAfter
		return generation
	}
	if c.mirrored == nil {
		c.mirrored = make(map[string]statedDefinition, len(mirrored))
	}
	for name, stated := range mirrored {
		c.mirrored[name] = stated
	}
	for _, tool := range excluded {
		delete(c.mirrored, tool.Name)
	}
	return generation
}

// knownMirrored returns the definition a listing stated for a tool addressed by
// name and the connection it was read over, reporting false when no listing has
// read one that settles it over the connection now standing, or when the one
// that did has outlived the lifetime the server gave it. The specification has
// a stale result read again the next time it is needed, and the headers of a
// call are what a definition is needed for.
//
// The record is dropped the moment the server announces a change, so whatever
// it holds was read at the count of changes now standing.
func (c *Client) knownMirrored(name string) (statedDefinition, int64, bool) {
	generation := c.proto.connectionGeneration()
	now := c.proto.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetOtherConnection(generation)
	if stated, known := c.mirrored[name]; known {
		stated.changes = c.changes
		return stated, generation, now.Before(stated.staleAfter)
	}
	// A whole catalogue that does not carry the name advertises nothing to
	// mirror under it, which is as settled an answer as a definition, and
	// stands for as long as every page it was read from does.
	settled := c.catalogued && now.Before(c.cataloguedUntil)
	return statedDefinition{staleAfter: c.cataloguedUntil, changes: c.changes}, generation, settled
}

// forgetOtherConnection drops the mirrored record when it was read over another
// connection than the one identified by generation. A definition is the server
// speaking, and a handshake settles who is speaking: a restarted server may
// declare another type for a mirrored property, or stop mirroring it at all,
// and a call weighed against what the previous one said would be refused
// locally for input the server would accept. Nothing reaches the server to
// correct it, since a call refused before it is sent earns no answer, so the
// record is dropped and the catalogue read again. The caller holds c.mu.
func (c *Client) forgetOtherConnection(generation int64) {
	if c.listedOn == generation {
		return
	}
	c.mirrored = nil
	c.catalogued = false
	c.cataloguedUntil = time.Time{}
	c.listedOn = generation
}

// CallTool invokes a tool by name. A nil arguments map is sent as an empty
// object.
//
// The input properties the tool's schema asks to be mirrored into request
// headers travel with the very first attempt: the definition comes from a
// listing this client has already read and the server still lets it keep, or
// from one it reads before the call.
// The headers are what an intermediary in front of the server routes and
// authorizes on, and a request that arrives without them may be refused by
// something that answers on HTTP's terms rather than the protocol's, so the
// call must not be sent in the hope of being told what it is missing.
//
// An optional Continuation repeats a call the server left unfinished, carrying
// the inputs it asked for; see UnfinishedResultError.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any, continuation ...Continuation) (*ToolResult, error) {
	if err := checkContinuation(continuation); err != nil {
		return nil, err
	}
	held, err := c.mirroredParametersOf(ctx, name)
	if err != nil {
		return nil, err
	}
	return c.callTool(ctx, name, arguments, held, continuation)
}

// heldDefinition is what a call weighs its mirrored headers against: what a
// listing stated for the tool, the connection that listing was read over, and
// whether it was read from the server for this very call. A definition just
// read states the terms the server states now, so a call it cannot render is
// refused on them rather than sending the client back for the same answer, and
// it is as current as the server can state it however short a lifetime it was
// given and whatever the server announced while it was being read: a server
// that gives its definitions no lifetime is asked for them before every call,
// not twice for one.
//
// bound reports that the definition describes one connection and no other. It
// is what tells an attempt travelling over a connection the definition was not
// read over to refuse rather than send: a definition that mirrors nothing
// refuses nothing by itself, so a server that has since begun mirroring a
// parameter would be sent the call without the header it now asks for. The same
// holds for a definition that has outlived its lifetime or the catalogue it was
// read from, whose server may have done the same while the connection stood.
type heldDefinition struct {
	statedDefinition
	generation int64
	bound      bool
	fresh      bool
}

// held records what a listing stated for a tool and the connection it was read
// over. A transport with no header channel carries no mirrored header whatever
// a definition says, so one read over it is bound to nothing: neither a
// reconnection nor a newer definition could change the headers of a call that
// has none.
func (c *Client) held(stated statedDefinition, generation int64) heldDefinition {
	if _, carriesHeaders := c.transport.(HeaderSender); !carriesHeaders {
		return heldDefinition{statedDefinition: statedDefinition{params: stated.params}}
	}
	return heldDefinition{statedDefinition: stated, generation: generation, bound: true}
}

// outlived reports whether a held definition no longer states what the server
// does: the connection it was read over has been replaced, the lifetime the
// server gave it has run out, or the server has announced a change to its
// catalogue since. It is asked by the attempt itself, which holds the exchange,
// so the connection it weighs is the one the attempt travels over and the time
// the one it is sent at.
func (c *Client) outlived(held heldDefinition) bool {
	if c.proto.connectionGeneration() != held.generation {
		return true
	}
	if held.fresh {
		return false
	}
	return !c.proto.now().Before(held.staleAfter) || c.catalogueChanges() != held.changes
}

// headersFor renders the mirrored headers of one attempt from the definition
// this call holds, deferred so the work happens only if the negotiated protocol
// carries headers at all.
//
// An attempt travelling over another connection than the definition was read
// over is refused before it is sent. A handshake settles who is speaking, and a
// server reached again may be another one: it may mirror a parameter the
// definition in hand says nothing about, and the call would reach it without
// the header an intermediary in front of it routes and authorizes on. The
// refusal is the client's own, so nothing has been sent and the catalogue can
// still be read again and the call repeated with what the server now states.
//
// An attempt whose definition has outlived the lifetime the server gave it, or
// the catalogue the server has since said it changed, is refused the same way,
// for the same reason: the specification has a stale result read again the next
// time it is needed, and an intermediary refusing the call on HTTP's terms
// would never say which header it went without.
func (c *Client) headersFor(held heldDefinition) headerFunc {
	if !held.bound && len(held.params) == 0 {
		return nil
	}
	return func(encoded json.RawMessage) (map[string]string, error) {
		if held.bound && c.outlived(held) {
			return nil, &staleDefinition{}
		}
		headers, err := held.params.headers(encoded)
		if err != nil {
			return nil, &mirrorRefusal{err: err}
		}
		return headers, nil
	}
}

// mirroredParametersOf reads the mirrored parameters of a tool addressed by
// name alone: from a listing this client has already read, or from the one it
// reads here. The catalogue is read once for as long as the server said it may
// be kept, not once per call, and nothing is read where its answer could not be
// used: a transport with no header channel carries no mirrored header, and
// neither does a revision that does not define them.
//
// The server refusing the listing on protocol terms is an answer: the catalogue
// is not to be had, and the call is made with what the caller stated rather
// than not at all. A failure of the channel is not an answer, and is reported
// to the caller: sending the call anyway would send it without the headers an
// intermediary routes on, which is the very thing the definition was read for.
//
// What it returns says which of the two it is, so a call the parameters refuse
// knows whether a newer definition could still be had.
//
// Whatever it returns belongs to one connection, which the exchange that read
// it reports: the revision is the one the connection found standing here was
// settled on, read together with the count that identifies it, and a call with
// nothing to mirror is held to the connection that had nothing to give it (see
// unstated).
func (c *Client) mirroredParametersOf(ctx context.Context, name string) (heldDefinition, error) {
	if _, carriesHeaders := c.transport.(HeaderSender); !carriesHeaders {
		return heldDefinition{}, nil
	}
	found, err := c.proto.connect(ctx)
	if err != nil {
		return heldDefinition{}, err
	}
	if handshakeFor(found.version) != handshakeDiscovery {
		return c.unstated(found.generation), nil
	}
	if stated, generation, known := c.knownMirrored(name); known {
		return c.held(stated, generation), nil
	}
	tool, advertised, generation, err := c.refreshedTool(ctx, name)
	switch {
	case isServerRefusal(err), err == nil && !advertised:
		return c.unstated(generation), nil
	case err != nil:
		return heldDefinition{}, err
	}
	return c.held(tool.stated(), tool.generation).freshlyRead(), nil
}

// unstated is what a call holds when the connection identified by generation
// had no definition to give it: the revision it was settled on mirrors nothing
// into headers, the server refused the listing on protocol terms, or its
// catalogue does not advertise the tool. The call is then made with what the
// caller stated, and over that connection alone.
//
// Having nothing to mirror is an answer like a definition is, given by one
// server to the caller whose credential the connection was settled under. The
// server reached by a later handshake may be another one, or the same one
// answering another caller, and may advertise the tool mirroring a parameter:
// sent there on the strength of what the connection before it lacked, the call
// would arrive without the header an intermediary routes and authorizes on. So
// the answer is bound to its connection as a definition is, and an attempt over
// another one is refused before it is sent and the catalogue read again. It was
// read for this very call, so nothing else outdates it.
func (c *Client) unstated(generation int64) heldDefinition {
	return c.held(statedDefinition{}, generation).freshlyRead()
}

// freshlyRead marks a held definition as one read from the server for the very
// call that holds it.
func (h heldDefinition) freshlyRead() heldDefinition {
	h.fresh = true
	return h
}

// isServerRefusal reports whether a failure is the server answering on protocol
// terms rather than the exchange itself failing. Such an answer leaves the
// connection standing, so the caller may go on and try something else.
func isServerRefusal(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr)
}

// callTool performs a tools/call with a known set of mirrored parameters. On a
// header mismatch it re-reads the tool's definition and repeats the call once,
// but only when the refreshed definition mirrors something the first attempt did
// not: retrying with the same headers would only fail the same way.
//
// A definition this client holds can also refuse the call before it is sent, by
// declaring for a mirrored property a type the argument does not carry. That
// refusal is weighed the same way, because a definition is only ever what the
// server last stated: one it has since changed refuses input the server would
// now accept, and no request reaches it to say so. The definition is read again
// and the call repeated with it, unless it was read for this very call, which is
// as current as the server can state it.
//
// An attempt refused for travelling over a connection the definition was not
// read over is weighed the same way again, and is read again however fresh the
// definition was: what settles it is the handshake that has happened since, not
// how recently the definition was read before it. So is one refused because the
// definition has outlived the lifetime the server gave it, or the catalogue the
// server has since announced a change to. None of these refusals reached the
// server, so whatever the definition read again asks for, including nothing at
// all, is the first set of headers this call puts on the wire. The definition
// read again was read for this very call, so the repeat is held to the
// connection it travels over and to nothing else: a server that gives its
// definitions no lifetime at all, or announces a change with every listing, is
// read once more, not for ever.
//
// The re-reads are weighed the way the one before the call is. A failure of the
// channel is reported as itself: the connection is down by then, and answering
// with the first outcome would tell the caller to fix its call when what it has
// to fix is the connection. The server refusing the listing on protocol terms,
// or no longer advertising the tool, leaves the first outcome standing as the
// answer to the call where there is one: a header the definition in hand could
// not render, or the server's own refusal of the headers it was sent. An attempt
// refused only because its definition had gone out of date has no outcome to
// leave standing, since nothing about the call itself was wrong, so it is made
// with what the caller stated, exactly as a call by name is when the catalogue
// has no definition to give it: over the connection that had none to give, and
// no other.
func (c *Client) callTool(ctx context.Context, name string, arguments map[string]any, held heldDefinition, continuation []Continuation) (*ToolResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	params := map[string]any{"name": name, "arguments": arguments}
	if err := applyContinuation(params, continuation); err != nil {
		return nil, err
	}

	result, err := c.dispatchToolCall(ctx, params, c.headersFor(held))
	refused := isMirrorRefusal(err)
	stale := isStaleDefinition(err)
	unsent := refused || stale
	if !unsent && !isHeaderMismatch(err) {
		return result, err
	}
	if refused && held.fresh {
		return nil, err
	}

	refreshed, found, generation, listErr := c.refreshedTool(ctx, name)
	switch {
	case listErr != nil && !isServerRefusal(listErr):
		return nil, listErr
	case (listErr != nil || !found) && stale:
		return c.dispatchToolCall(ctx, params, c.headersFor(c.unstated(generation)))
	case listErr != nil, !found:
		return nil, err
	}
	// A call the held definition could not render carries no headers yet, so
	// whatever the refreshed one renders, including none at all, is the first
	// set this call would put on the wire.
	if !unsent && !headersWouldDiffer(params, held.params, refreshed.mirrored) {
		return nil, err
	}
	current := c.held(refreshed.stated(), refreshed.generation).freshlyRead()
	return c.dispatchToolCall(ctx, params, c.headersFor(current))
}

// headersWouldDiffer reports whether the refreshed definition mirrors anything
// the definition the first attempt used did not. Both sets are rendered from
// one encoding of the call's params, so what is weighed is the same body read
// two ways rather than two encodings of it; the headers that travel are
// rendered by the attempt itself, from the body they travel with.
func headersWouldDiffer(params map[string]any, sent, refreshed mirroredParameters) bool {
	encoded, err := json.Marshal(params)
	if err != nil {
		return false
	}
	before, beforeErr := sent.headers(encoded)
	after, afterErr := refreshed.headers(encoded)
	if beforeErr != nil || afterErr != nil {
		return false
	}
	return !sameHeaders(before, after)
}

// dispatchToolCall sends one tools/call exchange and decodes its result.
func (c *Client) dispatchToolCall(ctx context.Context, params map[string]any, extra headerFunc) (*ToolResult, error) {
	raw, err := c.proto.dispatchWith(ctx, "tools/call", params, extra)
	if err != nil {
		return nil, err
	}
	return parseToolResult(raw)
}

// refreshedTool re-reads one tool's definition from the server, reporting false
// when the catalogue no longer advertises it and returning whatever failed the
// listing, which is the caller's to weigh.
//
// It also reports the connection its answer describes, since having no
// definition to give is the answer of one connection as much as a definition
// is. For a catalogue that was read, that is the connection the listing states.
// A listing the server refused brings back no result to state one, so it is the
// connection found standing when the listing was asked for. A handshake settled
// between the two can only make that the connection before the one that
// refused, never one after it, and an answer held to a connection already
// replaced is read again rather than sent on: the mistake it allows is a
// listing too many, and not a call sent on another connection's terms.
func (c *Client) refreshedTool(ctx context.Context, name string) (Tool, bool, int64, error) {
	found, err := c.proto.connect(ctx)
	if err != nil {
		return Tool{}, false, crossedGeneration, err
	}
	tools, generation, err := c.listTools(ctx, nil)
	if err != nil {
		return Tool{}, false, found.generation, err
	}
	for _, tool := range tools {
		if tool.Name == name {
			return tool, true, generation, nil
		}
	}
	return Tool{}, false, generation, nil
}

// staleDefinition marks an attempt this client refused to send because the
// definition it was weighed against no longer states what the server does: the
// connection the attempt would travel on is not the one the definition was read
// over, the definition has outlived the lifetime the server gave it, or the
// server has announced a change to its catalogue since. Nothing reached the
// server, so the connection stands and the definition can be read again over
// it.
type staleDefinition struct{}

// Error implements the error interface.
func (e *staleDefinition) Error() string {
	return "the tool definition this call mirrors no longer stands: the connection it was read over " +
		"has been replaced, the lifetime the server gave it has run out, or the server has changed its catalogue"
}

// isStaleDefinition reports whether a failure is this client refusing an
// attempt because the definition behind its headers is out of date.
func isStaleDefinition(err error) bool {
	var stale *staleDefinition
	return errors.As(err, &stale)
}

// mirrorRefusal marks a call this client refused to send because the definition
// it weighed the call against cannot render one of the headers that definition
// mirrors. It carries the failure unchanged, so the caller is told the same
// either way; the marker is only what tells the call that a definition it holds
// is what stood in the way, and that reading a newer one may clear it.
type mirrorRefusal struct{ err error }

// Error implements the error interface.
func (e *mirrorRefusal) Error() string { return e.err.Error() }

// Unwrap exposes the refusal itself for errors.Is/As.
func (e *mirrorRefusal) Unwrap() error { return e.err }

// isMirrorRefusal reports whether a failure is this client refusing to render
// the mirrored headers of a call. Nothing reached the server, so the connection
// stands and the definition can be read again.
func isMirrorRefusal(err error) bool {
	var refusal *mirrorRefusal
	return errors.As(err, &refusal)
}

// isHeaderMismatch reports whether a failure is the server refusing a request
// whose mirrored headers do not match its body.
func isHeaderMismatch(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == CodeHeaderMismatch
}

// Resources lists the server's resources, following pagination.
func (c *Client) Resources(ctx context.Context, limit ...int) ([]Resource, error) {
	entries, err := c.list(ctx, "resources", limit)
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(entries))
	for _, e := range entries {
		r, err := parseResource(e)
		if err != nil {
			return nil, err
		}
		resources = append(resources, r)
	}
	return resources, nil
}

// ReadResource reads a resource by URI. An optional Continuation repeats a read
// the server left unfinished, carrying the inputs it asked for; see
// UnfinishedResultError.
func (c *Client) ReadResource(ctx context.Context, uri string, continuation ...Continuation) (*ResourceReadResult, error) {
	params := map[string]any{"uri": uri}
	if err := applyContinuation(params, continuation); err != nil {
		return nil, err
	}
	raw, err := c.proto.dispatch(ctx, "resources/read", params)
	if err != nil {
		return nil, err
	}
	return parseResourceReadResult(raw)
}

// Prompts lists the server's prompts, following pagination.
func (c *Client) Prompts(ctx context.Context, limit ...int) ([]Prompt, error) {
	entries, err := c.list(ctx, "prompts", limit)
	if err != nil {
		return nil, err
	}
	prompts := make([]Prompt, 0, len(entries))
	for _, e := range entries {
		p, err := parsePrompt(e)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, nil
}

// GetPrompt fetches a prompt by name. A nil arguments map is sent as an empty
// object. An optional Continuation repeats a request the server left
// unfinished, carrying the inputs it asked for; see UnfinishedResultError.
func (c *Client) GetPrompt(ctx context.Context, name string, arguments map[string]any, continuation ...Continuation) (*PromptResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	params := map[string]any{"name": name, "arguments": arguments}
	if err := applyContinuation(params, continuation); err != nil {
		return nil, err
	}
	raw, err := c.proto.dispatch(ctx, "prompts/get", params)
	if err != nil {
		return nil, err
	}
	return parsePromptResult(raw)
}
