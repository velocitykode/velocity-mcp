package client

import (
	"bufio"
	"context"
	"encoding/json"
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
// JSON-RPC frame and queues the reply (a single JSON body, or one or more
// frames parsed from an SSE response) for Receive. What it puts on the wire
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

	// bearer is the token the client last settled, settled whether it has
	// settled one at all, and authorization the count of different tokens it
	// has settled, which is what identifies the authorization context of each.
	bearer        string
	settled       bool
	authorization int64
	// sessionBearer is the token the session was opened with.
	sessionBearer string
}

// Compile-time assertions that *HTTPTransport satisfies Transport and its
// optional protocol hooks.
var (
	_ Transport          = (*HTTPTransport)(nil)
	_ ProtocolAware      = (*HTTPTransport)(nil)
	_ HeaderSender       = (*HTTPTransport)(nil)
	_ AuthorizationAware = (*HTTPTransport)(nil)
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
		t.reset()
		return wrapError(err, "HTTP request to ["+t.url+"] failed")
	}
	defer resp.Body.Close()

	t.captureSessionID(resp, bearer)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		challenge := oauth.ParseChallenge(resp.Header.Get("WWW-Authenticate"))
		t.reset()
		return &oauth.AuthorizationRequiredError{
			Message:   "the server requires authorization (HTTP " + strconv.Itoa(resp.StatusCode) + ") for endpoint [" + t.url + "]",
			Challenge: challenge,
		}
	case resp.StatusCode == http.StatusNotFound && hadSession:
		t.reset()
		return errSessionExpired
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return t.handleUnsuccessful(resp)
	}

	t.mu.Lock()
	if t.version != "" && handshakeFor(t.version) == handshakeInitialize {
		t.initialized = true
	}
	t.mu.Unlock()

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return t.readSSE(resp.Body)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return wrapError(err, "unable to read response from ["+t.url+"]")
	}
	trimmed := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusAccepted || trimmed == "" {
		return nil
	}
	t.enqueue(trimmed)
	return nil
}

// Receive returns the next queued frame.
func (t *HTTPTransport) Receive(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) == 0 {
		return "", newError("no message available from the HTTP transport")
	}
	msg := t.queue[0]
	t.queue = t.queue[1:]
	return msg, nil
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
// queued for the caller to read; a status that says the endpoint is broken or
// absent is a client error; anything else means this endpoint would not take
// the request as sent, which is a transport failure the caller may retry
// differently.
func (t *HTTPTransport) handleUnsuccessful(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err == nil {
		for _, frame := range errorBodyFrames(resp.Header.Get("Content-Type"), string(body)) {
			if hasJSONRPCError(frame) {
				t.enqueue(frame)
				return nil
			}
		}
	}

	status := strconv.Itoa(resp.StatusCode)
	t.reset()
	if resp.StatusCode == http.StatusNotFound ||
		(resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented) {
		return newError("unexpected HTTP status [" + status + "] from endpoint [" + t.url + "]")
	}
	return NewTransportError("the endpoint ["+t.url+"] rejected the request with HTTP status ["+status+"]", nil)
}

// errorBodyFrames returns the JSON-RPC frames an unsuccessful response body
// carries. Streamable HTTP lets a server answer a POST with a single JSON
// document or with an event stream, and an unsuccessful status is no exception:
// a rejection sent as a data frame is the server answering on protocol terms
// just as much as one sent as a plain body, and reading only the latter would
// turn it into a transport failure the caller cannot act on.
func errorBodyFrames(contentType, body string) []string {
	if !strings.Contains(contentType, "text/event-stream") {
		if trimmed := strings.TrimSpace(body); trimmed != "" {
			return []string{trimmed}
		}
		return nil
	}
	var frames []string
	var event sseEvent
	for _, line := range strings.Split(body, "\n") {
		if data, ok := event.line(line); ok {
			frames = append(frames, data)
		}
	}
	if data, ok := event.flush(); ok {
		frames = append(frames, data)
	}
	return frames
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

// readSSE parses an event stream, queueing the data of each event as one frame.
// A server-initiated request over the stream is rejected: this client does not
// service inbound requests on the HTTP transport.
func (t *HTTPTransport) readSSE(body io.Reader) error {
	reader := bufio.NewReader(body)
	var event sseEvent
	for {
		line, err := reader.ReadString('\n')
		data, complete := event.line(line)
		if err != nil {
			// The stream is over, so whatever the last event gathered is all
			// there is of it; a fragment cut short decodes as no frame at all.
			if !complete {
				data, complete = event.flush()
			}
		}
		if complete {
			if isServerRequest(data) {
				t.reset()
				return newError("the server initiated a request over the SSE stream, which this HTTP client does not support")
			}
			t.enqueue(data)
		}
		if err != nil {
			return nil
		}
	}
}

// isServerRequest reports whether an SSE data frame is a server-initiated
// JSON-RPC request (carries both method and id) rather than a response.
func isServerRequest(data string) bool {
	var probe struct {
		Method *string         `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return false
	}
	return probe.Method != nil && len(probe.ID) > 0
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
// and the receive queue. The token a client settled stands: it belongs to the
// exchange in progress, which may open the channel again before it is done.
func (t *HTTPTransport) reset() {
	t.mu.Lock()
	t.version = ""
	t.sessionID = ""
	t.sessionBearer = ""
	t.initialized = false
	t.queue = nil
	t.mu.Unlock()
}
