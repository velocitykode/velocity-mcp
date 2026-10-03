package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/velocitykode/velocity/crypto"
	"github.com/velocitykode/velocity/httpclient"

	"github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// maxErrorBodyBytes caps how much of an unsuccessful response body is read
// while deciding whether it carries a JSON-RPC error. The body is untrusted and
// only the envelope matters, so a server cannot make the client buffer an
// unbounded error page.
const maxErrorBodyBytes = 64 << 10

// HTTPTransport speaks streamable HTTP to an MCP server: each Send POSTs one
// JSON-RPC frame and makes the reply available to Receive, a single JSON body
// as one frame and an event stream one event at a time as each arrives, so a
// response the server has flushed is delivered whether or not the server then
// ends the stream. What it puts on the wire
// depends on the protocol version of the exchange: a discovery-era request
// carries the protocol version header and the headers mirroring the frame and
// never a session, while an initialize-era request carries the session the
// server assigned and, once initialized, the negotiated version. It surfaces a
// 401/403 as an oauth.AuthorizationRequiredError and a post-session 404 as a
// session-expiry signal.
//
// The bearer token is the credential the transport presents, and so the
// authorization context of everything fetched with it. A client settles it once
// for each exchange through SettleAuthorization, and every frame of that
// exchange presents the token it resolved; a transport driven without a client
// resolves it for each request, as it always has. A session is released with
// the token that opened it, whatever token is presented by then.
type HTTPTransport struct {
	url string

	mu          sync.Mutex
	timeout     time.Duration
	token       func() string
	version     ProtocolVersion
	sessionID   string
	initialized bool
	queue       []string
	client      *httpclient.Client
	// stream is the event stream the last reply with a body opened, read one
	// event at a time by Receive. It is nil when the last reply was a single
	// JSON document or carried nothing.
	stream *openStream

	// bearer is the token the client last settled, settled whether it has
	// settled one at all, and authorization the count of different tokens it
	// has settled, which is what identifies the authorization context of each.
	bearer        string
	settled       bool
	authorization int64
	// sessionBearer is the token the session was opened with.
	sessionBearer string
	// replyStatus is the HTTP status of the last reply, which is what says how
	// a JSON-RPC error read off that reply is to be weighed by the probe.
	replyStatus int
}

// Compile-time assertions that *HTTPTransport satisfies Transport and its
// optional protocol hooks.
var (
	_ Transport          = (*HTTPTransport)(nil)
	_ ProtocolAware      = (*HTTPTransport)(nil)
	_ HeaderSender       = (*HTTPTransport)(nil)
	_ AuthorizationAware = (*HTTPTransport)(nil)
	_ CancellationAware  = (*HTTPTransport)(nil)
	_ repliesPerRequest  = (*HTTPTransport)(nil)
	_ progressDeaf       = (*HTTPTransport)(nil)
	_ eraDetecting       = (*HTTPTransport)(nil)
)

// NewHTTPTransport builds an HTTP transport targeting url. Private-IP denial is
// disabled because the target is an operator-supplied MCP endpoint (commonly
// localhost during development); the OAuth subsystem applies its own host checks
// to server-advertised URLs.
func NewHTTPTransport(url string) *HTTPTransport {
	t := &HTTPTransport{url: url, timeout: defaultTimeout}
	t.client = t.buildClient()
	return t
}

// buildClient constructs the underlying velocity httpclient for the current
// timeout.
func (t *HTTPTransport) buildClient() *httpclient.Client {
	return httpclient.New(
		httpclient.WithTimeout(t.timeout),
		httpclient.WithoutPrivateIPDeny(),
	)
}

// WithToken sets a static bearer token sent on every request.
func (t *HTTPTransport) WithToken(token string) {
	t.WithTokenFunc(func() string { return token })
}

// WithTokenFunc sets a callback resolving the bearer token, allowing the caller
// to refresh or rotate it. A client resolves it once for each exchange; a
// transport driven without one resolves it for each request.
func (t *HTTPTransport) WithTokenFunc(fn func() string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = fn
}

// SettleAuthorization resolves the bearer token the frames that follow present
// and returns the authorization context it belongs to. The tokens are compared
// in constant time: they are secrets, and the comparison is made on every
// exchange.
func (t *HTTPTransport) SettleAuthorization() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	bearer := t.resolveLocked()
	if !t.settled || !crypto.EqualString(bearer, t.bearer) {
		t.authorization++
	}
	t.bearer, t.settled = bearer, true
	return t.authorization
}

// AuthorizationContext returns the authorization context the token resolved now
// belongs to, settling nothing.
func (t *HTTPTransport) AuthorizationContext() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.settled && crypto.EqualString(t.resolveLocked(), t.bearer) {
		return t.authorization
	}
	return t.authorization + 1
}

// resolveLocked asks the token callback for the bearer token, which is empty
// when there is no callback or it has none to give. The caller holds t.mu.
func (t *HTTPTransport) resolveLocked() string {
	if t.token == nil {
		return ""
	}
	return t.token()
}

// presented returns the bearer token the next frame presents: the one a client
// settled for the exchange in progress, or, without a client, whatever the
// callback resolves now.
func (t *HTTPTransport) presented() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.settled {
		return t.bearer
	}
	return t.resolveLocked()
}

// URL returns the configured server URL.
func (t *HTTPTransport) URL() string { return t.url }

// SetTimeout sets the request timeout and rebuilds the underlying client.
func (t *HTTPTransport) SetTimeout(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timeout = d
	t.client = t.buildClient()
}

// Timeout returns the request timeout, which the client also holds a whole
// exchange to.
func (t *HTTPTransport) Timeout() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timeout
}

// Recipe returns the transport's serializable description. The token is captured
// by value at recipe time.
func (t *HTTPTransport) Recipe() Recipe {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Recipe{Driver: "http", URL: t.url, Token: t.resolveLocked(), Timeout: t.timeout}
}

// Connect resets per-session state. The HTTP transport has no persistent
// connection; sessions are established by the initialize exchange.
func (t *HTTPTransport) Connect(ctx context.Context) error {
	t.reset()
	return nil
}

// Disconnect terminates the server session (best effort) and resets state.
func (t *HTTPTransport) Disconnect() error {
	t.terminateSession()
	t.reset()
	return nil
}

// Send POSTs a frame and queues the server's reply for Receive.
func (t *HTTPTransport) Send(ctx context.Context, message string) error {
	return t.SendWithHeaders(ctx, message, nil)
}

// SendWithHeaders POSTs a frame together with the protocol headers mirroring it
// and queues the server's reply for Receive. The headers are sent as given; a
// header the transport owns cannot be displaced by one of them.
func (t *HTTPTransport) SendWithHeaders(ctx context.Context, message string, headers map[string]string) error {
	t.mu.Lock()
	hadSession := t.sessionID != ""
	client := t.client
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(message))
	if err != nil {
		return wrapError(err, "unable to build request to ["+t.url+"]")
	}
	bearer := t.presented()
	t.applyHeaders(req, headers, bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(ctx, req)
	if err != nil {
		return t.failed(ctx, err, "HTTP request to ["+t.url+"] failed")
	}

	t.captureSessionID(resp, bearer)
	t.mu.Lock()
	t.replyStatus = resp.StatusCode
	t.mu.Unlock()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_ = resp.Body.Close()
		challenge := oauth.ParseChallenge(resp.Header.Get("WWW-Authenticate"))
		t.reset()
		return &oauth.AuthorizationRequiredError{
			Message:   "the server requires authorization (HTTP " + strconv.Itoa(resp.StatusCode) + ") for endpoint [" + t.url + "]",
			Challenge: challenge,
		}
	case resp.StatusCode == http.StatusNotFound && hadSession:
		_ = resp.Body.Close()
		t.reset()
		return errSessionExpired
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		defer resp.Body.Close()
		return t.handleUnsuccessful(resp)
	}

	t.mu.Lock()
	if t.version != "" && handshakeFor(t.version) == handshakeInitialize {
		t.initialized = true
	}
	t.mu.Unlock()

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The stream is the reply to this frame and is read as its events
		// arrive. The stream of the exchange before, if the server left it
		// open, is over: nothing of it is waited for any more.
		t.openStream(ctx, resp.Body)
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return t.failed(ctx, err, "unable to read response from ["+t.url+"]")
	}
	trimmed := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusAccepted || trimmed == "" {
		// A request answered with nothing has no reply to read. The stream of
		// the exchange before it, if the server left one open, is not its
		// reply either: left in place, the next Receive would read a stream
		// whose exchange is over and report how that one ended. A notification
		// or a response the client posts in the middle of an exchange is
		// accepted this way too, and the stream it was read from stays.
		if isRequestFrame(message) {
			t.closeStream()
		}
		return nil
	}
	t.closeStream()
	t.enqueue(trimmed)
	return nil
}

// isRequestFrame reports whether a frame is a request: it names a method and
// carries an id, so a reply is owed to it.
func isRequestFrame(message string) bool {
	var frame struct {
		Method *string         `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(message), &frame); err != nil {
		return false
	}
	return frame.Method != nil && len(frame.ID) != 0
}

// Receive returns the next frame: one queued from a JSON reply, or the next
// event of the open stream, read as it arrives. A stream the server ends with
// nothing more to say leaves no message available, which is what an empty reply
// leaves too.
//
// The stream is read under the context the frame that opened it was sent with,
// which the protocol makes the context of the exchange: a caller withdrawing
// its request closes the stream, which streamable HTTP makes the cancellation
// signal, and a deadline that passes is a timeout like any other.
func (t *HTTPTransport) Receive(ctx context.Context) (string, error) {
	t.mu.Lock()
	if len(t.queue) > 0 {
		msg := t.queue[0]
		t.queue = t.queue[1:]
		t.mu.Unlock()
		return msg, nil
	}
	stream := t.stream
	t.mu.Unlock()
	if stream == nil {
		return "", newError("no message available from the HTTP transport")
	}

	data, err := stream.next()
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, io.EOF):
		t.dropStream(stream)
		return "", newError("no message available from the HTTP transport")
	default:
		t.dropStream(stream)
		return "", t.failed(stream.ctx, err, "unable to read the response stream from ["+t.url+"]")
	}
}

// failed classifies a failure of the channel and reports it typed, so a caller
// can tell a timeout, a withdrawn request, and a broken channel apart as it can
// over stdio. A request the caller withdrew is neither a timeout nor a failure
// of the channel, and the session stands: the specification makes closing the
// stream the cancellation of that one request. A deadline that passed, the
// transport's own or the caller's, is a timeout, and the session stands for the
// same reason. Anything else is the channel failing, which takes the
// negotiated state with it.
//
// A timeout and a failed channel are both marked as unanswered: the endpoint
// said nothing, so neither is read as the answer of a server that does not know
// the request, which only a status that refuses it is.
func (t *HTTPTransport) failed(ctx context.Context, err error, message string) error {
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		return wrapError(err, "the wait for a response from ["+t.url+"] was cancelled")
	case isTimeout(ctx, err):
		return NewTimeoutError("timed out while waiting for a response from ["+t.url+"]", &unanswered{err: err})
	default:
		t.reset()
		return NewTransportError(message, &unanswered{err: err})
	}
}

// isTimeout reports whether a failure is a deadline passing: the context's own,
// or the one the HTTP client enforces on the whole request, body included,
// which it reports through the Timeout method of the error.
func isTimeout(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// openStream is an event stream a reply opened, read one event at a time.
type openStream struct {
	// ctx is the context the frame that opened the stream was sent with, which
	// bounds every read of the stream.
	ctx    context.Context
	body   io.ReadCloser
	reader *bufio.Reader
	event  sseEvent
}

// next returns the data of the next event carrying any, or io.EOF once the
// stream has ended with nothing more to deliver.
func (s *openStream) next() (string, error) {
	for {
		line, err := s.reader.ReadString('\n')
		data, complete := s.event.line(line)
		if err != nil {
			// The stream is over, so whatever the last event gathered is all
			// there is of it; a fragment cut short decodes as no frame at all.
			if !complete {
				data, complete = s.event.flush()
			}
			if complete {
				return data, nil
			}
			return "", err
		}
		if complete {
			return data, nil
		}
	}
}

// openStream installs the stream a reply opened in place of whatever stream
// stood before, which belonged to an exchange that is over.
func (t *HTTPTransport) openStream(ctx context.Context, body io.ReadCloser) {
	stream := &openStream{ctx: ctx, body: body, reader: bufio.NewReader(body)}
	t.mu.Lock()
	previous := t.stream
	t.stream = stream
	t.mu.Unlock()
	if previous != nil {
		_ = previous.body.Close()
	}
}

// closeStream closes the open stream, if any, and forgets it.
func (t *HTTPTransport) closeStream() {
	t.mu.Lock()
	stream := t.stream
	t.stream = nil
	t.mu.Unlock()
	if stream != nil {
		_ = stream.body.Close()
	}
}

// dropStream closes a stream that has ended or failed, and forgets it unless
// another reply has replaced it since.
func (t *HTTPTransport) dropStream(stream *openStream) {
	t.mu.Lock()
	if t.stream == stream {
		t.stream = nil
	}
	t.mu.Unlock()
	_ = stream.body.Close()
}

// progressDeaf marks that progress reported for a request cannot keep it alive
// over this transport: the HTTP client bounds one whole request, the body of
// its reply included, by the timeout, so a call is cut there however much
// progress the server reports. Requests therefore ask for none. Removing this
// method is what switches the asking on, once a reply stream is bounded by the
// exchange instead.
func (t *HTTPTransport) progressDeaf() {}

// repliesPerRequest marks that the reply to a request arrives on the response
// to the POST that carried it, and on no other: a reply the server sends late
// has nowhere to arrive once that response is closed.
func (t *HTTPTransport) repliesPerRequest() {}

// NotifiesCancellation reports how a request abandoned at the given version is
// withdrawn. The revision without a session makes closing the response stream
// the cancellation of the request it answers, which the context of the exchange
// has done by the time the wait returns. The revisions before it say the
// opposite, that a closed stream is not to be read as a cancellation, and have
// the client send the notification.
func (t *HTTPTransport) NotifiesCancellation(version ProtocolVersion) bool {
	return handshakeFor(version) == handshakeInitialize
}

// UseProtocol records the protocol version the following frames belong to.
// Crossing from one handshake to the other invalidates the session and the
// initialized state: they belong to the handshake that established them and
// mean nothing to the other.
func (t *HTTPTransport) UseProtocol(version ProtocolVersion) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.version != "" && handshakeFor(t.version) != handshakeFor(version) {
		t.sessionID = ""
		t.sessionBearer = ""
		t.initialized = false
	}
	t.version = version
}

// applyHeaders sets the Accept, protocol, mirrored, and Authorization headers
// for a request. bearer is the token the request presents, resolved once by the
// caller so that what the request states and what is recorded of it agree.
func (t *HTTPTransport) applyHeaders(req *http.Request, mirrored map[string]string, bearer string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	req.Header.Set("Accept", "application/json, text/event-stream")

	if t.version != "" && handshakeFor(t.version) == handshakeDiscovery {
		// The discovery handshake is sessionless: the version travels on every
		// request, from the very first one.
		req.Header.Set(protocolVersionHeader, t.version)
	} else {
		if t.sessionID != "" {
			req.Header.Set(sessionHeader, t.sessionID)
		}
		// The version is only known once the server has answered initialize, so
		// the handshake request itself carries no version header.
		if t.initialized && t.version != "" {
			req.Header.Set(protocolVersionHeader, t.version)
		}
	}

	for name, value := range mirrored {
		// The version and the session describe the connection, not the frame:
		// a mirrored header can never displace what the transport negotiated.
		if transportOwnedHeader(name) {
			continue
		}
		req.Header.Set(name, value)
	}

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
}

// transportOwnedHeader reports whether a header describes the connection
// itself, which only the transport may set.
func transportOwnedHeader(name string) bool {
	return strings.EqualFold(name, protocolVersionHeader) || strings.EqualFold(name, sessionHeader)
}

// captureSessionID records a session id advertised in the response, together
// with the token the request that opened it presented. Only the initialize
// handshake has sessions; a session id returned to a discovery-era request is
// ignored.
func (t *HTTPTransport) captureSessionID(resp *http.Response, bearer string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.version != "" && handshakeFor(t.version) == handshakeDiscovery {
		return
	}
	if id := resp.Header.Get(sessionHeader); id != "" {
		t.sessionID = id
		t.sessionBearer = bearer
	}
}

// handleUnsuccessful decides what an unsuccessful response means. A body
// carrying a JSON-RPC error is the server answering on protocol terms, so it is
// queued for the caller to read, unless the status is one that describes the
// moment rather than the request (see passing). A bare 400 is a transport
// failure: the endpoint did not take the request as it was put, and the caller
// may put it another way, which is how the connection probe reaches a server
// that predates it. Every other status is a client error and
// says nothing of what the endpoint speaks.
func (t *HTTPTransport) handleUnsuccessful(resp *http.Response) error {
	if !passing(resp.StatusCode) {
		if frame, found := firstErrorFrame(resp.Header.Get("Content-Type"), io.LimitReader(resp.Body, maxErrorBodyBytes)); found {
			t.closeStream()
			t.enqueue(frame)
			return nil
		}
	}

	status := strconv.Itoa(resp.StatusCode)
	t.reset()
	if !refusesTheRequest(resp.StatusCode) {
		return newError("unexpected HTTP status [" + status + "] from endpoint [" + t.url + "]")
	}
	return NewTransportError("the endpoint ["+t.url+"] rejected the request with HTTP status ["+status+"]", nil)
}

// passing reports whether a status describes this moment rather than the
// request: the caller is asked to slow down or to come back (408, 425, 429), or
// what stands in front of the server could not reach it (502, 503, 504). Such a
// status is the answer whatever body comes with it. A JSON-RPC error in that
// body is not read as the server answering on protocol terms: the connection
// probe takes an error it does not recognize for the answer of a server that
// predates it, and a rate limit worded as JSON-RPC would settle the older
// handshake with a server that speaks the newer one.
func passing(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// refusesTheRequest reports whether a status is the one the specification has
// a server of the older handshake answer a modern request with. The streamable
// HTTP binding of 2026-07-28 names it: on 400 Bad Request a client inspects the
// body, and "if the body is empty or is not a recognized modern JSON-RPC error,
// fall back to initialize".
//
// No other status is read that way. A rate limit, a conflict, a method the
// endpoint does not allow, or an upstream that failed say nothing of which
// handshake the server speaks, and a client that took one for the server's era
// would settle the older handshake with a server that speaks the newer one,
// and go on offering it.
func refusesTheRequest(status int) bool {
	return status == http.StatusBadRequest
}

// probeAnsweredAsLegacy weighs a JSON-RPC error the probe was answered with,
// which is not one of the errors a modern server defines, and reports whether
// it is the answer of a server that speaks the older handshake. Two answers
// are: any such error under 400 Bad Request, which is the fallback the binding
// names, and method-not-found under any status, since a modern server must
// implement the request the probe makes and one that does not know it is not
// modern. Any other error, an internal error under 200 or 500 for one, is a
// server that failed at the request, not one that predates it.
func (t *HTTPTransport) probeAnsweredAsLegacy(rejection *jsonrpc.Error) bool {
	t.mu.Lock()
	status := t.replyStatus
	t.mu.Unlock()
	return refusesTheRequest(status) || rejection.Code == jsonrpc.CodeMethodNotFound
}

// firstErrorFrame returns the first JSON-RPC error frame an unsuccessful
// response body carries, reporting false when it carries none. Streamable HTTP
// lets a server answer a POST with a single JSON document or with an event
// stream, and an unsuccessful status is no exception: a rejection sent as a
// data frame is the server answering on protocol terms just as much as one sent
// as a plain body, and reading only the latter would turn it into a transport
// failure the caller cannot act on. A stream is read event by event and left
// the moment the rejection is found, so a server that keeps it open afterwards
// holds nothing up; the body is bounded by the caller.
func firstErrorFrame(contentType string, body io.Reader) (string, bool) {
	if !strings.Contains(contentType, "text/event-stream") {
		content, err := io.ReadAll(body)
		if err != nil {
			return "", false
		}
		trimmed := strings.TrimSpace(string(content))
		return trimmed, trimmed != "" && hasJSONRPCError(trimmed)
	}
	stream := &openStream{reader: bufio.NewReader(body)}
	for {
		data, err := stream.next()
		if err != nil {
			return "", false
		}
		if hasJSONRPCError(data) {
			return data, true
		}
	}
}

// sseEvent gathers the data lines of one event stream event. The event stream
// interpretation rules append the value of every data field to the event's data
// buffer, separated by a line feed, and dispatch the event at the blank line
// that ends it, so an event whose JSON is written across several data lines is
// one frame rather than several.
type sseEvent struct {
	data    strings.Builder
	started bool
}

// line feeds one line of the stream, returning the data of the event the line
// completes and reporting false while the event is still being gathered.
func (e *sseEvent) line(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return e.flush()
	}
	if value, ok := sseDataField(line); ok {
		if e.started {
			e.data.WriteByte('\n')
		}
		e.started = true
		e.data.WriteString(value)
	}
	return "", false
}

// flush returns the data gathered so far and starts a fresh event, reporting
// false when nothing carrying content was gathered.
func (e *sseEvent) flush() (string, bool) {
	data := e.data.String()
	e.data.Reset()
	e.started = false
	data = strings.TrimSpace(data)
	return data, data != ""
}

// sseDataField returns the value of an event stream data field, reporting false
// for a comment, for any other field, and for a line that is not a field at all.
// The field name runs up to the first colon and a single space after the colon
// is part of the separator rather than of the value.
func sseDataField(line string) (string, bool) {
	name, value, separated := strings.Cut(line, ":")
	if name != "data" {
		return "", false
	}
	if !separated {
		return "", true
	}
	return strings.TrimPrefix(value, " "), true
}

// hasJSONRPCError reports whether a body is a JSON-RPC envelope carrying an
// error object. The MCP specification requires the error member of a response
// to be an object, so a member that is null, a string, or an array does not make
// the body a protocol answer: the status decides what such a response means.
func hasJSONRPCError(content string) bool {
	var probe struct {
		JSONRPC string          `json:"jsonrpc"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(content), &probe); err != nil {
		return false
	}
	if probe.JSONRPC != jsonrpc.Version || len(probe.Error) == 0 {
		return false
	}
	var object map[string]any
	if err := json.Unmarshal(probe.Error, &object); err != nil {
		return false
	}
	// A JSON null decodes into a nil map without failing, so the decode alone
	// does not prove an object was there.
	return object != nil
}

// terminateSession issues a best-effort DELETE to release the server session.
// The session is released with the token that opened it: the token presented by
// now may be another caller's, and the session is not theirs to be shown.
func (t *HTTPTransport) terminateSession() {
	t.mu.Lock()
	sessionID, bearer, client := t.sessionID, t.sessionBearer, t.client
	t.mu.Unlock()
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	t.applyHeaders(req, nil, bearer)
	if resp, err := client.Do(ctx, req); err == nil {
		_ = resp.Body.Close()
	}
}

// enqueue appends a frame to the receive queue.
func (t *HTTPTransport) enqueue(message string) {
	t.mu.Lock()
	t.queue = append(t.queue, message)
	t.mu.Unlock()
}

// reset clears the negotiated version, the session, the initialization flag,
// the receive queue, and the open stream. The token a client settled stands: it
// belongs to the exchange in progress, which may open the channel again before
// it is done.
func (t *HTTPTransport) reset() {
	t.mu.Lock()
	t.version = ""
	t.sessionID = ""
	t.sessionBearer = ""
	t.initialized = false
	t.queue = nil
	stream := t.stream
	t.stream = nil
	t.mu.Unlock()
	if stream != nil {
		_ = stream.body.Close()
	}
}
