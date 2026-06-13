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

	"github.com/velocitykode/velocity/httpclient"

	"github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity-mcp/server"
)

// sessionHeader is the case-insensitive header carrying the MCP session id.
const sessionHeader = "Mcp-Session-Id"

// protocolVersionHeader advertises the negotiated protocol version on requests
// made after initialize.
const protocolVersionHeader = "Mcp-Protocol-Version"

// HTTPTransport speaks streamable HTTP to an MCP server: each Send POSTs one
// JSON-RPC frame and queues the reply (a single JSON body, or one or more
// frames parsed from an SSE response) for Receive. It tracks the server-assigned
// session id and surfaces a 401/403 as an oauth.AuthorizationRequiredError and a
// post-session 404 as a session-expiry signal.
type HTTPTransport struct {
	url string

	mu          sync.Mutex
	timeout     time.Duration
	token       func() string
	sessionID   string
	initialized bool
	queue       []string
	client      *httpclient.Client
}

// Compile-time assertion that *HTTPTransport satisfies Transport.
var _ Transport = (*HTTPTransport)(nil)

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

// WithTokenFunc sets a callback resolving the bearer token per request, allowing
// the caller to refresh or rotate it.
func (t *HTTPTransport) WithTokenFunc(fn func() string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = fn
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
	token := ""
	if t.token != nil {
		token = t.token()
	}
	return Recipe{Driver: "http", URL: t.url, Token: token, Timeout: t.timeout}
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
	t.mu.Lock()
	hadSession := t.sessionID != ""
	client := t.client
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(message))
	if err != nil {
		return wrapError(err, "unable to build request to ["+t.url+"]")
	}
	t.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(ctx, req)
	if err != nil {
		t.reset()
		return wrapError(err, "HTTP request to ["+t.url+"] failed")
	}
	defer resp.Body.Close()

	t.captureSessionID(resp)

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
		t.reset()
		return newError("unexpected HTTP status [" + strconv.Itoa(resp.StatusCode) + "] from endpoint [" + t.url + "]")
	}

	t.mu.Lock()
	t.initialized = true
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

// applyHeaders sets the Accept, session, protocol-version, and Authorization
// headers for a request.
func (t *HTTPTransport) applyHeaders(req *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	req.Header.Set("Accept", "application/json, text/event-stream")
	if t.sessionID != "" {
		req.Header.Set(sessionHeader, t.sessionID)
	}
	if t.initialized {
		req.Header.Set(protocolVersionHeader, server.LatestProtocolVersion)
	}
	if t.token != nil {
		if tok := t.token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
}

// captureSessionID records a session id advertised in the response.
func (t *HTTPTransport) captureSessionID(resp *http.Response) {
	if id := resp.Header.Get(sessionHeader); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
}

// readSSE parses an event stream, queueing each data frame. A server-initiated
// request over the stream is rejected: this client does not service inbound
// requests on the HTTP transport.
func (t *HTTPTransport) readSSE(body io.Reader) error {
	reader := bufio.NewReader(body)
	for {
		line, err := reader.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data:"); ok {
			data = strings.TrimSpace(data)
			if data != "" {
				if isServerRequest(data) {
					t.reset()
					return newError("the server initiated a request over the SSE stream, which this HTTP client does not support")
				}
				t.enqueue(data)
			}
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
func (t *HTTPTransport) terminateSession() {
	t.mu.Lock()
	sessionID, client := t.sessionID, t.client
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
	t.applyHeaders(req)
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

// reset clears the session, initialization flag, and receive queue.
func (t *HTTPTransport) reset() {
	t.mu.Lock()
	t.sessionID = ""
	t.initialized = false
	t.queue = nil
	t.mu.Unlock()
}
