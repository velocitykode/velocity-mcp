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

	// mu guards what the client keeps of the server's tools: the tools the
	// last listing refused, the catalogue, and changes, the count of the times
	// the server has said its catalogue changed. What is kept belongs to the
	// count it was read at: the specification has that notification outdate a
	// listing at once, however much of its lifetime is left.
	//
	// excludedFor is the authorization context the listing that refused those
	// tools was asked for in. Which tools a server advertises may depend on who
	// is asking, so the names it refused are shown in that context alone. The
	// catalogue needs no such mark: a change of credential is a change of
	// connection, and it is dropped with the connection it was read over.
	mu          sync.Mutex
	excluded    []ExcludedTool
	excludedFor int64
	catalogue   catalogue
	changes     int64
}

// toolsChangedNotification is the notification a server sends when the list of
// tools it advertises has changed.
const toolsChangedNotification = "notifications/tools/list_changed"

// catalogue is what the client keeps of the tools one connection stated, which
// is what a call by name is mirrored from. It describes the server that stated
// it and nothing beyond: generation is the connection it was read over, and a
// handshake settled since has it dropped.
//
// whole reports that a listing read the catalogue to its end, which is what
// makes a name it does not carry a name the server does not advertise, and
// wholeUntil the moment that stops being so: the first of its pages to go stale
// takes the claim with it.
type catalogue struct {
	generation int64
	tools      map[string]definition
	whole      bool
	wholeUntil time.Time
}

// definition is what a listing stated for one tool: the parameters its schema
// asks to be mirrored into headers, or the reason this client refuses the
// schema, and the moment the page that stated it stops being fresh. The server
// gave the page that lifetime, and past it the definition is what the server
// said once rather than what it says.
type definition struct {
	params     mirroredParameters
	refused    error
	staleAfter time.Time
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
	c.catalogue = catalogue{generation: c.catalogue.generation}
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

// WithTimeout sets the timeout of a request whose context carries no deadline
// of its own. It runs from the request going out, and only a progress
// notification for that request starts it over: a server sending log
// notifications, or progress on something else, cannot keep the client waiting
// past it (see WithMaxTimeout for the bound that holds regardless of progress).
// The transport is given the same value as its own bound on the wait for any
// one frame. A timeout set on the
// transport itself, as the recipe of a named client sets it, bounds the
// exchange the same way.
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.transport.SetTimeout(d)
	c.proto.setTimeout(d)
	return c
}

// WithMaxTimeout sets the longest a request whose context carries no deadline
// may take, whatever progress the server reports for it. The timeout itself
// starts over each time the server reports progress on the request, so a call
// that is being worked on is not cut while it says so; the maximum is what ends
// a request that goes on reporting progress without ever finishing. It defaults
// to ten times the timeout, and a duration of zero or less restores that. A
// deadline on the caller's context is the bound in place of both.
func (c *Client) WithMaxTimeout(d time.Duration) *Client {
	c.proto.setMaxTimeout(d)
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
	// The count is read before the first page is asked for, so a change the
	// server announces while the listing is being read outdates it as well:
	// nothing says which of its pages the server had already put together.
	changes := c.catalogueChanges()
	read, err := c.listOn(ctx, "tools", limit)
	if err != nil {
		return nil, err
	}
	tools, stated, excluded, err := c.toolsOf(read)
	if err != nil {
		return nil, err
	}
	c.recordListing(stated, excluded, len(limit) == 0, read, changes)
	return tools, nil
}

// toolsOf decodes the entries of a tools listing: the tools this client offers,
// what the listing stated for every tool by name, the refused ones included,
// and the tools it refused with the reasons.
func (c *Client) toolsOf(read listing) ([]Tool, map[string]definition, []ExcludedTool, error) {
	tools := make([]Tool, 0, len(read.entries))
	stated := make(map[string]definition, len(read.entries))
	var excluded []ExcludedTool
	for _, e := range read.entries {
		t, err := parseTool(c, e.payload)
		if err != nil {
			return nil, nil, nil, err
		}
		stated[t.Name] = definition{params: t.mirrored, refused: t.mirrorErr, staleAfter: e.staleAfter}
		if t.mirrorErr != nil {
			excluded = append(excluded, ExcludedTool{Name: t.Name, Err: t.mirrorErr})
			continue
		}
		tools = append(tools, t)
	}
	return tools, stated, excluded, nil
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

// recordListing records what one listing found: the tools it refused, and what
// it stated for each tool. A whole listing is the catalogue as it now stands
// and replaces what was known, so a tool the server has dropped leaves nothing
// behind for a later call to mirror. A capped listing is only a part of the
// catalogue, so what it did carry is merged into the rest.
//
// The record belongs to the connection the listing was read over, which the
// listing states: the one every page travelled over, or crossedGeneration when
// they did not all travel over one. A listing that crossed a handshake read its
// pages from more than one server, so it records nothing. What an earlier
// listing settled over the connection now standing is left as it was, being the
// one thing here that describes it.
//
// A listing the server outdated while it was being read, by announcing that its
// catalogue had changed, records nothing either: changes is the count of such
// announcements the listing set out at.
func (c *Client) recordListing(stated map[string]definition, excluded []ExcludedTool, whole bool, read listing, changes int64) {
	generation := c.proto.connectionGeneration()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetOtherConnection(generation)
	c.excluded, c.excludedFor = excluded, read.authorization
	if read.readOn != generation || changes != c.changes {
		return
	}
	if whole {
		c.catalogue = catalogue{generation: generation, tools: stated, whole: true, wholeUntil: read.staleAfter}
		return
	}
	if c.catalogue.tools == nil {
		c.catalogue.tools = make(map[string]definition, len(stated))
	}
	for name, definition := range stated {
		c.catalogue.tools[name] = definition
	}
}

// statedFor returns what the catalogue kept of the connection identified by
// generation states for a tool, reporting false when it states nothing that
// still stands: no listing has read the tool over that connection, or the one
// that did has outlived the lifetime the server gave it. The specification has
// a stale result read again the next time it is needed, and the headers of a
// call are what a definition is needed for.
//
// The record is dropped the moment the server announces a change, so whatever
// it holds was read at the count of changes now standing.
func (c *Client) statedFor(name string, generation int64) (definition, bool) {
	now := c.proto.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetOtherConnection(generation)
	if stated, known := c.catalogue.tools[name]; known {
		return stated, now.Before(stated.staleAfter)
	}
	// A whole catalogue that does not carry the name advertises nothing to
	// mirror under it, which is as settled an answer as a definition, and
	// stands for as long as every page it was read from does.
	return definition{}, c.catalogue.whole && now.Before(c.catalogue.wholeUntil)
}

// forgetOtherConnection drops the catalogue when it was read over another
// connection than the one identified by generation. A definition is the server
// speaking, and a handshake settles who is speaking: a restarted server may
// declare another type for a mirrored property, or stop mirroring it at all,
// and the server reached under another credential may advertise other tools
// altogether. The caller holds c.mu.
func (c *Client) forgetOtherConnection(generation int64) {
	if c.catalogue.generation == generation {
		return
	}
	c.catalogue = catalogue{generation: generation}
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
// A tool whose definition this client refuses, as ExcludedTools reports it, is
// not called at all: the definition is invalid, the headers it asks for cannot
// be told from it, and the call would go out without them.
//
// An optional Continuation repeats a call the server left unfinished, carrying
// the inputs it asked for; see UnfinishedResultError.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any, continuation ...Continuation) (*ToolResult, error) {
	if err := checkContinuation(continuation); err != nil {
		return nil, err
	}
	return c.callTool(ctx, name, arguments, continuation)
}

// callTool performs a tools/call, mirrored from the definition the server
// states for the tool over the connection the call travels on.
//
// A server that refuses the headers it was sent, with a header mismatch, may
// have changed the definition since the client read it. The definition is read
// again and the call repeated once, but only when what it now mirrors differs
// from what was sent: repeating the call with the same headers would only fail
// the same way. Where no newer definition is to be had the server's refusal
// stands as the answer to the call.
func (c *Client) callTool(ctx context.Context, name string, arguments map[string]any, continuation []Continuation) (*ToolResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	params := map[string]any{"name": name, "arguments": arguments}
	if err := applyContinuation(params, continuation); err != nil {
		return nil, err
	}
	// A transport with no header channel carries no mirrored header whatever a
	// definition says, so nothing is read where its answer could not be used.
	if _, carriesHeaders := c.transport.(HeaderSender); !carriesHeaders {
		return c.dispatchToolCall(ctx, params, nil)
	}

	first := &mirroredAttempt{}
	result, err := c.dispatchToolCall(ctx, params, c.mirroring(name, first, nil))
	if !isHeaderMismatch(err) || !first.rendered {
		return result, err
	}
	again, repeatErr := c.dispatchToolCall(ctx, params, c.mirroring(name, &mirroredAttempt{}, first))
	if errors.Is(repeatErr, errNothingNewToMirror) {
		return nil, err
	}
	return again, repeatErr
}

// mirroredAttempt is what one attempt at a tools/call put into mirrored
// headers: whether it rendered any at all, which only a revision that carries
// headers does, and the headers it rendered.
type mirroredAttempt struct {
	rendered bool
	headers  map[string]string
}

// errNothingNewToMirror ends the repeat of a call the server refused for its
// headers when reading the definition again brought nothing the first attempt
// did not already send. Nothing is sent, and the refusal stands.
var errNothingNewToMirror = errors.New("client: the definition read again mirrors what was already sent")

// mirroring renders the mirrored headers of one attempt at calling a tool by
// name, and records them in attempt. It runs with the exchange held, over the
// connection the call is about to travel on, so the definition it mirrors from
// is that connection's: one the catalogue kept of it still states, or one read
// over it now, in the same exchange, under the same credential. Nothing can
// replace the connection between the definition being read and the call being
// sent, so a call is never mirrored from what another server, or the same
// server answering another caller, stated.
//
// What the catalogue kept is only ever what the server last stated. When a call
// cannot be rendered from it, because it declares for a mirrored property a
// type the argument does not carry or because this client refuses the
// definition, the catalogue is read again before the call is refused: the
// server may have changed the definition since, and no request would reach it
// to say so. A definition read for this very call is as current as the server
// can state it, and a call it cannot render is refused on it.
//
// The server refusing the listing on protocol terms is an answer: the catalogue
// is not to be had, and the call is made with what the caller stated rather
// than not at all, as it is when the catalogue does not advertise the tool. A
// failure of the channel is not an answer and fails the call: sending it anyway
// would send it without the headers an intermediary routes on.
//
// rejected is the attempt the server refused for its headers, when this one
// repeats it: the definition is then read again whatever the catalogue kept,
// and the attempt is given up with errNothingNewToMirror unless it mirrors
// something the rejected one did not.
func (c *Client) mirroring(name string, attempt, rejected *mirroredAttempt) mirrorFunc {
	return func(ctx context.Context, under heldConnection, encoded json.RawMessage) (map[string]string, error) {
		stated, known := definition{}, false
		if rejected == nil {
			stated, known = c.statedFor(name, under.generation)
		}
		read := !known
		if read {
			var advertised bool
			var err error
			stated, advertised, err = c.readDefinition(ctx, under, name)
			switch {
			case err != nil && !isServerRefusal(err):
				return nil, err
			case (err != nil || !advertised) && rejected != nil:
				return nil, errNothingNewToMirror
			}
		}

		headers, err := stated.headers(name, encoded)
		if err != nil && !read {
			current, advertised, readErr := c.readDefinition(ctx, under, name)
			switch {
			case readErr != nil && !isServerRefusal(readErr):
				return nil, readErr
			case readErr != nil || !advertised:
				// No newer definition is to be had, so the refusal of the one
				// in hand is the answer to the call.
				return nil, err
			}
			headers, err = current.headers(name, encoded)
		}
		if err != nil {
			return nil, err
		}
		if rejected != nil && sameHeaders(headers, rejected.headers) {
			return nil, errNothingNewToMirror
		}
		attempt.rendered, attempt.headers = true, headers
		return headers, nil
	}
}

// headers renders the mirrored headers of a call to the tool from its encoded
// params. A definition this client refuses renders none and reports why: the
// annotations it carries are invalid, so which headers the server expects
// cannot be told from it, and a call sent without them is the non-conforming
// request the specification describes.
func (d definition) headers(name string, encoded json.RawMessage) (map[string]string, error) {
	if d.refused != nil {
		return nil, wrapError(d.refused, "tool ["+name+"] cannot be called: its definition is invalid")
	}
	return d.params.headers(encoded)
}

// readDefinition reads the catalogue over the held connection and returns what
// it states for one tool, reporting false when it does not advertise the tool.
// Whatever fails the listing is returned for the caller to weigh. The catalogue
// read is recorded as any whole listing is, so the calls after this one find it.
//
// The exchange is held while the catalogue is read, so the whole of the reading
// is given the time one request is: a server that pages its catalogue slowly
// cannot hold the gate for a timeout a page.
func (c *Client) readDefinition(ctx context.Context, under heldConnection, name string) (definition, bool, error) {
	ctx, cancel := c.proto.boundedOnce(ctx)
	defer cancel()
	changes := c.catalogueChanges()
	read, err := readPages(ctx, under.asked, "tools", 0, false)
	if err != nil {
		return definition{}, false, err
	}
	_, stated, excluded, err := c.toolsOf(read)
	if err != nil {
		return definition{}, false, err
	}
	c.recordListing(stated, excluded, true, read, changes)
	found, advertised := stated[name]
	return found, advertised, nil
}

// isServerRefusal reports whether a failure is the server answering on protocol
// terms rather than the exchange itself failing. Such an answer leaves the
// connection standing, so the caller may go on and try something else.
func isServerRefusal(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr)
}

// dispatchToolCall sends one tools/call exchange and decodes its result.
func (c *Client) dispatchToolCall(ctx context.Context, params map[string]any, extra mirrorFunc) (*ToolResult, error) {
	raw, err := c.proto.dispatchWith(ctx, "tools/call", params, extra)
	if err != nil {
		return nil, err
	}
	return parseToolResult(raw)
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
