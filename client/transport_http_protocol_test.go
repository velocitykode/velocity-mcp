package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// recordedRequest is one request an httptest MCP endpoint received.
type recordedRequest struct {
	// httpMethod is the HTTP verb (a session is terminated with DELETE).
	httpMethod string
	method     string
	id         json.RawMessage
	headers    http.Header
	body       string
}

// recordingEndpoint is an httptest server that records every request and
// answers each one from a handler the test supplies.
type recordingEndpoint struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

// newRecordingEndpoint starts an endpoint that records every request and lets
// reply write the response for it.
func newRecordingEndpoint(t *testing.T, reply func(w http.ResponseWriter, request recordedRequest)) *recordingEndpoint {
	t.Helper()
	endpoint := &recordingEndpoint{}
	endpoint.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &probe)
		request := recordedRequest{
			httpMethod: r.Method,
			method:     probe.Method,
			id:         probe.ID,
			headers:    r.Header.Clone(),
			body:       string(body),
		}
		endpoint.mu.Lock()
		endpoint.requests = append(endpoint.requests, request)
		endpoint.mu.Unlock()
		reply(w, request)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// request returns the recorded request carrying the given JSON-RPC method.
func (e *recordingEndpoint) request(t *testing.T, method string) recordedRequest {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, request := range e.requests {
		if request.method == method {
			return request
		}
	}
	t.Fatalf("the endpoint never received a %q request", method)
	return recordedRequest{}
}

// lastRequest returns the most recent recorded request carrying the given
// JSON-RPC method, for a test that sends the same method more than once.
func (e *recordingEndpoint) lastRequest(t *testing.T, method string) recordedRequest {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := len(e.requests) - 1; index >= 0; index-- {
		if e.requests[index].method == method {
			return e.requests[index]
		}
	}
	t.Fatalf("the endpoint never received a %q request", method)
	return recordedRequest{}
}

// requestsFor returns every recorded request carrying the given JSON-RPC
// method, in order.
func (e *recordingEndpoint) requestsFor(method string) []recordedRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recordedRequest
	for _, request := range e.requests {
		if request.method == method {
			out = append(out, request)
		}
	}
	return out
}

// methods returns the JSON-RPC method of every recorded request, in order.
func (e *recordingEndpoint) methods() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	methods := make([]string, 0, len(e.requests))
	for _, request := range e.requests {
		methods = append(methods, request.method)
	}
	return methods
}

// writeJSON writes a JSON-RPC frame, echoing the id of the request in flight.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// jsonFrame renders a response frame correlating to the id of the request in
// flight.
func jsonFrame(id json.RawMessage, members map[string]any) string {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	frame := map[string]any{"jsonrpc": "2.0", "id": id}
	for key, value := range members {
		frame[key] = value
	}
	out, err := json.Marshal(frame)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func TestHTTPTransportDiscoveryEraHeaders(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		// A session id offered to a sessionless exchange must be ignored.
		w.Header().Set(sessionHeader, "sess-ignored")
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})

	tr := NewHTTPTransport(endpoint.URL)
	tr.UseProtocol(LatestProtocolVersion)
	ctx := context.Background()

	if err := tr.SendWithHeaders(ctx, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, map[string]string{
		methodHeader: "server/discover",
	}); err != nil {
		t.Fatalf("send discover: %v", err)
	}
	if _, err := tr.Receive(ctx); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if err := tr.SendWithHeaders(ctx, `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`, map[string]string{
		methodHeader: "tools/call",
		nameHeader:   "execute_sql",
	}); err != nil {
		t.Fatalf("send call: %v", err)
	}

	for _, method := range []string{"server/discover", "tools/call"} {
		headers := endpoint.request(t, method).headers
		if got := headers.Get(protocolVersionHeader); got != LatestProtocolVersion {
			t.Fatalf("%s %s = %q, want %q", method, protocolVersionHeader, got, LatestProtocolVersion)
		}
		if got := headers.Get(sessionHeader); got != "" {
			t.Fatalf("%s carried a session header %q; the discovery handshake is sessionless", method, got)
		}
		if got := headers.Get(methodHeader); got != method {
			t.Fatalf("%s %s = %q, want %q", method, methodHeader, got, method)
		}
	}
	if got := endpoint.request(t, "tools/call").headers.Get(nameHeader); got != "execute_sql" {
		t.Fatalf("%s = %q, want %q", nameHeader, got, "execute_sql")
	}
	if got := endpoint.request(t, "server/discover").headers.Values(nameHeader); len(got) != 0 {
		t.Fatalf("discover carried %s = %v", nameHeader, got)
	}

	// A sessionless exchange has no session to terminate, so disconnecting
	// must not go back to the endpoint.
	_ = tr.Disconnect()
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	for _, request := range endpoint.requests {
		if request.httpMethod == http.MethodDelete {
			t.Fatal("disconnecting a sessionless exchange sent a session termination")
		}
	}
}

func TestHTTPTransportKeepsOwnershipOfTheConnectionHeaders(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})

	tr := NewHTTPTransport(endpoint.URL)
	tr.UseProtocol(LatestProtocolVersion)

	err := tr.SendWithHeaders(context.Background(), `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, map[string]string{
		methodHeader:           "tools/call",
		nameHeader:             "execute_sql",
		"mcp-protocol-version": "2999-01-01",
		"MCP-Session-Id":       "forged-session",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	headers := endpoint.request(t, "tools/call").headers
	if got := headers.Get(protocolVersionHeader); got != LatestProtocolVersion {
		t.Fatalf("%s = %q, want the negotiated %q", protocolVersionHeader, got, LatestProtocolVersion)
	}
	if got := headers.Get(sessionHeader); got != "" {
		t.Fatalf("%s = %q, want no session on a sessionless exchange", sessionHeader, got)
	}
	if got := headers.Get(nameHeader); got != "execute_sql" {
		t.Fatalf("%s = %q", nameHeader, got)
	}
}

func TestHTTPTransportInitializeEraHeaders(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		w.Header().Set(sessionHeader, "sess-abc")
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})

	tr := NewHTTPTransport(endpoint.URL)
	tr.UseProtocol(ProtocolV20251125)
	ctx := context.Background()

	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); err != nil {
		t.Fatalf("send initialize: %v", err)
	}
	if _, err := tr.Receive(ctx); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); err != nil {
		t.Fatalf("send list: %v", err)
	}

	initialize := endpoint.request(t, "initialize").headers
	if got := initialize.Get(protocolVersionHeader); got != "" {
		t.Fatalf("the initialize request carried %s = %q; no version is negotiated yet", protocolVersionHeader, got)
	}
	if got := initialize.Get(sessionHeader); got != "" {
		t.Fatalf("the initialize request carried a session header %q", got)
	}

	list := endpoint.request(t, "tools/list").headers
	if got := list.Get(protocolVersionHeader); got != ProtocolV20251125 {
		t.Fatalf("tools/list %s = %q, want %q", protocolVersionHeader, got, ProtocolV20251125)
	}
	if got := list.Get(sessionHeader); got != "sess-abc" {
		t.Fatalf("tools/list %s = %q, want the captured session", sessionHeader, got)
	}
	if got := list.Get(methodHeader); got != "" {
		t.Fatalf("a legacy request carried %s = %q", methodHeader, got)
	}
}

func TestHTTPTransportDropsTheSessionWhenTheHandshakeChanges(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		w.Header().Set(sessionHeader, "sess-abc")
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})

	tr := NewHTTPTransport(endpoint.URL)
	ctx := context.Background()

	tr.UseProtocol(ProtocolV20251125)
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); err != nil {
		t.Fatalf("send initialize: %v", err)
	}
	if _, err := tr.Receive(ctx); err != nil {
		t.Fatalf("receive: %v", err)
	}

	// Another version of the same handshake keeps the session.
	tr.UseProtocol(ProtocolV20250618)
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	if got := endpoint.request(t, "ping").headers.Get(sessionHeader); got != "sess-abc" {
		t.Fatalf("ping %s = %q, want the session to survive a version change", sessionHeader, got)
	}
	if got := endpoint.request(t, "ping").headers.Get(protocolVersionHeader); got != ProtocolV20250618 {
		t.Fatalf("ping %s = %q, want %q", protocolVersionHeader, got, ProtocolV20250618)
	}

	// Crossing to the other handshake drops it.
	tr.UseProtocol(LatestProtocolVersion)
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); err != nil {
		t.Fatalf("send list: %v", err)
	}
	if got := endpoint.request(t, "tools/list").headers.Get(sessionHeader); got != "" {
		t.Fatalf("tools/list carried session %q across a handshake change", got)
	}
	if got := endpoint.request(t, "tools/list").headers.Get(protocolVersionHeader); got != LatestProtocolVersion {
		t.Fatalf("tools/list %s = %q, want %q", protocolVersionHeader, got, LatestProtocolVersion)
	}

	// The session was dropped, not merely left out of the sessionless request:
	// crossing back to the initialize handshake starts over with no session and
	// no version, exactly as a first initialize does.
	tr.UseProtocol(ProtocolV20251125)
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":4,"method":"initialize"}`); err != nil {
		t.Fatalf("send second initialize: %v", err)
	}
	second := endpoint.lastRequest(t, "initialize")
	if !strings.Contains(second.body, `"id":4`) {
		t.Fatalf("the endpoint did not record the second initialize, got %s", second.body)
	}
	if got := second.headers.Get(sessionHeader); got != "" {
		t.Fatalf("the second initialize carried the dropped session %q", got)
	}
	if got := second.headers.Get(protocolVersionHeader); got != "" {
		t.Fatalf("the second initialize carried %s = %q; no version is negotiated yet", protocolVersionHeader, got)
	}
}

// TestHTTPTransportDoesNotTerminateASessionItDropped is the other half of the
// drop: once the exchange has crossed to the sessionless handshake there is no
// server session left to release, so disconnecting must not go back to the
// endpoint with a termination for a session id it no longer holds.
func TestHTTPTransportDoesNotTerminateASessionItDropped(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		w.Header().Set(sessionHeader, "sess-abc")
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})

	tr := NewHTTPTransport(endpoint.URL)
	ctx := context.Background()

	tr.UseProtocol(ProtocolV20251125)
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); err != nil {
		t.Fatalf("send initialize: %v", err)
	}
	if _, err := tr.Receive(ctx); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got := endpoint.request(t, "initialize").headers.Get(sessionHeader); got != "" {
		t.Fatalf("the initialize request carried a session header %q", got)
	}

	tr.UseProtocol(LatestProtocolVersion)
	_ = tr.Disconnect()

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	for _, request := range endpoint.requests {
		if request.httpMethod == http.MethodDelete {
			t.Fatalf("disconnecting sent a session termination carrying %q",
				request.headers.Get(sessionHeader))
		}
	}
}

func TestHTTPTransportUnsuccessfulResponses(t *testing.T) {
	protocolError := jsonFrame(json.RawMessage("1"), map[string]any{
		"error": map[string]any{"code": CodeUnsupportedProtocolVersion, "message": "Unsupported protocol version"},
	})
	indentedError := indentJSON(t, protocolError)

	tests := []struct {
		name string
		// status, contentType, and body are what the endpoint answers with.
		// An empty contentType is answered as application/json.
		status      int
		contentType string
		body        string
		// wantQueued is the frame the transport queues for Receive.
		wantQueued string
		// wantErr is the exact error message the send fails with.
		wantErr string
		// wantTransportErr asserts the failure is a transport failure, which
		// the probe retries with the older handshake.
		wantTransportErr bool
	}{
		{
			name:       "a protocol error in a rejected body is queued for the caller",
			status:     http.StatusBadRequest,
			body:       protocolError,
			wantQueued: protocolError,
		},
		{
			// Streamable HTTP lets a server answer a POST with an event stream,
			// and an unsuccessful status is no exception: the rejection in the
			// data frame is the server answering on protocol terms.
			name:        "a protocol error sent as an event stream is queued for the caller",
			status:      http.StatusBadRequest,
			contentType: "text/event-stream",
			body:        "event: message\ndata: " + protocolError + "\n\n",
			wantQueued:  protocolError,
		},
		{
			// The event stream interpretation rules join the values of the data
			// fields of one event with a line feed, so a rejection written as
			// pretty-printed JSON is one frame and not one per line.
			name:        "a protocol error written across several data lines is one frame",
			status:      http.StatusBadRequest,
			contentType: "text/event-stream",
			body:        "event: message\n" + sseDataLines(indentedError) + "\n",
			wantQueued:  indentedError,
		},
		{
			name:        "a protocol error across several data lines ending the body is still queued",
			status:      http.StatusBadRequest,
			contentType: "text/event-stream",
			body:        "event: message\n" + sseDataLines(indentedError),
			wantQueued:  indentedError,
		},
		{
			// A data field with no value contributes an empty line to the
			// event, which is part of the payload rather than the end of it.
			name:        "an empty data line inside an event does not end it",
			status:      http.StatusBadRequest,
			contentType: "text/event-stream",
			body:        "data:\n" + sseDataLines(indentedError) + "\n",
			wantQueued:  indentedError,
		},
		{
			name:        "a protocol error after stream padding is still queued",
			status:      http.StatusBadRequest,
			contentType: "text/event-stream; charset=utf-8",
			body: ": keep-alive\nretry: 1000\n\n" +
				"event: message\nid: 7\ndata: " + protocolError + "\n\n",
			wantQueued: protocolError,
		},
		{
			name:        "the first protocol error of the stream is the answer",
			status:      http.StatusBadRequest,
			contentType: "text/event-stream",
			body: "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n" +
				"data: " + protocolError + "\n\n",
			wantQueued: protocolError,
		},
		{
			name:             "an event stream carrying no protocol error is a transport rejection",
			status:           http.StatusBadRequest,
			contentType:      "text/event-stream",
			body:             "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n",
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "an event stream carrying nothing is a transport rejection",
			status:           http.StatusBadRequest,
			contentType:      "text/event-stream",
			body:             ": keep-alive\n\n",
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "a method not allowed is a transport rejection",
			status:           http.StatusMethodNotAllowed,
			body:             "Method Not Allowed",
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [405]",
			wantTransportErr: true,
		},
		{
			name:             "a conflict is a transport rejection",
			status:           http.StatusConflict,
			body:             "Conflict",
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [409]",
			wantTransportErr: true,
		},
		{
			name:             "a gateway rejection with a non protocol body is a transport rejection",
			status:           http.StatusBadRequest,
			body:             `{"error":{"message":"invalid request"}}`,
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "a null error member is not a protocol answer",
			status:           http.StatusBadRequest,
			body:             `{"jsonrpc":"2.0","id":1,"error":null}`,
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "a string error member is not a protocol answer",
			status:           http.StatusBadRequest,
			body:             `{"jsonrpc":"2.0","id":1,"error":"Unsupported protocol version"}`,
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "an array error member is not a protocol answer",
			status:           http.StatusBadRequest,
			body:             `{"jsonrpc":"2.0","id":1,"error":[]}`,
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:   "an error envelope at another JSON-RPC version is not a protocol answer",
			status: http.StatusBadRequest,
			body: `{"jsonrpc":"1.0","id":1,"error":{"code":-32022,` +
				`"message":"Unsupported protocol version"}}`,
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "a body that is not JSON at all is not a protocol answer",
			status:           http.StatusBadRequest,
			body:             "<html><body>Bad Request</body></html>",
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [400]",
			wantTransportErr: true,
		},
		{
			name:             "a not implemented status is a transport rejection",
			status:           http.StatusNotImplemented,
			body:             "Not Implemented",
			wantErr:          "the endpoint [%s] rejected the request with HTTP status [501]",
			wantTransportErr: true,
		},
		{
			name:    "an unavailable endpoint is not a transport rejection",
			status:  http.StatusBadGateway,
			body:    "Bad Gateway",
			wantErr: "unexpected HTTP status [502] from endpoint [%s]",
		},
		{
			name:    "a server error is not a transport rejection",
			status:  http.StatusInternalServerError,
			body:    "boom",
			wantErr: "unexpected HTTP status [500] from endpoint [%s]",
		},
		{
			name:    "a not found without a session is not a transport rejection",
			status:  http.StatusNotFound,
			body:    "",
			wantErr: "unexpected HTTP status [404] from endpoint [%s]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				contentType := tc.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			tr := NewHTTPTransport(endpoint.URL)
			tr.UseProtocol(LatestProtocolVersion)

			err := tr.Send(context.Background(), `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`)

			if tc.wantQueued != "" {
				if err != nil {
					t.Fatalf("send: %v", err)
				}
				queued, err := tr.Receive(context.Background())
				if err != nil {
					t.Fatalf("receive: %v", err)
				}
				if queued != tc.wantQueued {
					t.Fatalf("queued frame = %q, want %q", queued, tc.wantQueued)
				}
				return
			}

			if err == nil {
				t.Fatal("expected the send to fail")
			}
			want := strings.ReplaceAll(tc.wantErr, "%s", endpoint.URL)
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}

			var transportErr *TransportError
			if errors.As(err, &transportErr) != tc.wantTransportErr {
				t.Fatalf("errors.As(*TransportError) = %v, want %v for %v", !tc.wantTransportErr, tc.wantTransportErr, err)
			}
			var clientErr *Error
			if !errors.As(err, &clientErr) {
				t.Fatalf("error %v is not a client error", err)
			}
		})
	}
}

func TestWebClientNegotiatesOverHTTP(t *testing.T) {
	tests := []struct {
		name string
		// discoverStatus and discoverBody are how the endpoint answers the
		// probe.
		discoverStatus int
		discoverBody   func(id json.RawMessage) string
		// initializeVersion is the version the endpoint settles the legacy
		// handshake on, whatever the client offered. It defaults to 2025-11-25.
		initializeVersion ProtocolVersion
		wantVersion       ProtocolVersion
		wantMethods       []string
	}{
		{
			name:           "a discovery server settles without a handshake",
			discoverStatus: http.StatusOK,
			discoverBody: func(id json.RawMessage) string {
				return jsonFrame(id, map[string]any{"result": map[string]any{
					"supportedVersions": []string{LatestProtocolVersion},
					"capabilities":      map[string]any{},
				}})
			},
			wantVersion: LatestProtocolVersion,
			wantMethods: []string{"server/discover", "tools/list", "tools/call"},
		},
		{
			name:           "an endpoint that refuses the probe falls back",
			discoverStatus: http.StatusMethodNotAllowed,
			discoverBody:   func(json.RawMessage) string { return "Method Not Allowed" },
			wantVersion:    ProtocolV20251125,
			wantMethods:    []string{"server/discover", "initialize", "notifications/initialized", "tools/call"},
		},
		{
			name:           "an endpoint that refuses the probe with an empty error envelope falls back",
			discoverStatus: http.StatusBadRequest,
			// The error member is null, so the body says nothing on protocol
			// terms and the status is what classifies the rejection.
			discoverBody: func(id json.RawMessage) string { return jsonFrame(id, map[string]any{"error": nil}) },
			wantVersion:  ProtocolV20251125,
			wantMethods:  []string{"server/discover", "initialize", "notifications/initialized", "tools/call"},
		},
		{
			name:           "a server that rejects the probe on protocol terms is negotiated down",
			discoverStatus: http.StatusBadRequest,
			discoverBody: func(id json.RawMessage) string {
				return jsonFrame(id, map[string]any{"error": map[string]any{
					"code":    CodeUnsupportedProtocolVersion,
					"message": "Unsupported protocol version",
					"data":    map[string]any{"supported": []string{ProtocolV20251125}},
				}})
			},
			wantVersion: ProtocolV20251125,
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized", "tools/call"},
		},
		{
			// The fallback offers 2025-11-25 unpinned, so an endpoint that only
			// speaks 2025-06-18 settles there and every request after the
			// handshake carries that version.
			name:              "an endpoint that settles the fallback on an older version is spoken to at that version",
			discoverStatus:    http.StatusMethodNotAllowed,
			discoverBody:      func(json.RawMessage) string { return "Method Not Allowed" },
			initializeVersion: ProtocolV20250618,
			wantVersion:       ProtocolV20250618,
			wantMethods:       []string{"server/discover", "initialize", "notifications/initialized", "tools/call"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settled := tc.initializeVersion
			if settled == "" {
				settled = ProtocolV20251125
			}
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				switch request.method {
				case "server/discover":
					writeJSON(w, tc.discoverStatus, tc.discoverBody(request.id))
				case "initialize":
					w.Header().Set(sessionHeader, "sess-http")
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"protocolVersion": settled,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "Test Server", "version": "1.0.0"},
					}}))
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"tools": []any{},
					}}))
				default:
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "done"}},
						"isError": false,
					}}))
				}
			})

			client := Web(endpoint.URL)
			result, err := client.CallTool(context.Background(), "execute_sql", map[string]any{"query": "select 1"})
			if err != nil {
				t.Fatalf("call tool: %v", err)
			}
			if result.Text() != "done" {
				t.Fatalf("tool result = %q", result.Text())
			}

			version, err := client.ProtocolVersion(context.Background())
			if err != nil {
				t.Fatalf("protocol version: %v", err)
			}
			if version != tc.wantVersion {
				t.Fatalf("negotiated version = %q, want %q", version, tc.wantVersion)
			}
			if got := endpoint.methods(); !slices.Equal(got, tc.wantMethods) {
				t.Fatalf("requests = %v, want %v", got, tc.wantMethods)
			}

			if tc.wantVersion != LatestProtocolVersion {
				// The fallback offers 2025-11-25 without pinning it; what the
				// endpoint settled on is what every frame after it speaks.
				initialize := endpoint.request(t, "initialize")
				if !strings.Contains(initialize.body, `"protocolVersion":"`+ProtocolV20251125+`"`) {
					t.Fatalf("initialize body = %s, want it to offer %q", initialize.body, ProtocolV20251125)
				}
				notification := endpoint.request(t, "notifications/initialized").headers
				if got := notification.Get(protocolVersionHeader); got != tc.wantVersion {
					t.Fatalf("notifications/initialized %s = %q, want %q", protocolVersionHeader, got, tc.wantVersion)
				}
				if got := notification.Get(sessionHeader); got != "sess-http" {
					t.Fatalf("notifications/initialized %s = %q, want the captured session", sessionHeader, got)
				}
			}

			call := endpoint.request(t, "tools/call").headers
			if tc.wantVersion == LatestProtocolVersion {
				if got := call.Get(methodHeader); got != "tools/call" {
					t.Fatalf("tools/call %s = %q", methodHeader, got)
				}
				if got := call.Get(nameHeader); got != "execute_sql" {
					t.Fatalf("tools/call %s = %q", nameHeader, got)
				}
				if got := call.Get(sessionHeader); got != "" {
					t.Fatalf("tools/call carried a session header %q", got)
				}
			} else {
				if got := call.Get(methodHeader); got != "" {
					t.Fatalf("a legacy tools/call carried %s = %q", methodHeader, got)
				}
				if got := call.Get(sessionHeader); got != "sess-http" {
					t.Fatalf("tools/call %s = %q, want the captured session", sessionHeader, got)
				}
			}
			if got := call.Get(protocolVersionHeader); got != tc.wantVersion {
				t.Fatalf("tools/call %s = %q, want %q", protocolVersionHeader, got, tc.wantVersion)
			}
		})
	}
}

// errorEnvelopeOfSize renders a well-formed JSON-RPC error envelope of exactly
// size bytes, padding the message to reach it.
func errorEnvelopeOfSize(t *testing.T, size int) string {
	t.Helper()
	const prefix = `{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"`
	const suffix = `"}}`
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("an error envelope cannot be rendered in %d bytes", size)
	}
	return prefix + strings.Repeat("A", padding) + suffix
}

// TestHTTPTransportCapsTheRejectedBodyItReads pins the cap on how much of an
// unsuccessful body the transport buffers while deciding whether it is a
// protocol answer. The bodies here are well-formed JSON-RPC error envelopes, so
// only the cap decides the outcome: one that fits is queued for the caller,
// while one that does not is truncated, no longer parses, and is classified by
// the status alone.
func TestHTTPTransportCapsTheRejectedBodyItReads(t *testing.T) {
	tests := []struct {
		name string
		// size is the exact length of the error envelope the endpoint answers
		// with.
		size int
		// wantQueued asserts the envelope reached the caller untouched.
		wantQueued bool
	}{
		{
			name:       "an envelope just inside the cap is a protocol answer",
			size:       maxErrorBodyBytes - 1,
			wantQueued: true,
		},
		{
			name:       "an envelope exactly at the cap is a protocol answer",
			size:       maxErrorBodyBytes,
			wantQueued: true,
		},
		{
			name: "an envelope one byte past the cap is truncated and not a protocol answer",
			size: maxErrorBodyBytes + 1,
		},
		{
			name: "an envelope far past the cap is truncated and not a protocol answer",
			size: maxErrorBodyBytes * 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := errorEnvelopeOfSize(t, tc.size)
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				writeJSON(w, http.StatusBadRequest, body)
			})

			tr := NewHTTPTransport(endpoint.URL)
			err := tr.Send(context.Background(), `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`)

			if tc.wantQueued {
				if err != nil {
					t.Fatalf("send: %v", err)
				}
				queued, err := tr.Receive(context.Background())
				if err != nil {
					t.Fatalf("receive: %v", err)
				}
				if queued != body {
					t.Fatalf("queued frame is %d bytes, want the %d byte envelope", len(queued), len(body))
				}
				return
			}

			if err == nil {
				t.Fatal("expected the oversized body to be rejected on transport terms")
			}
			want := "the endpoint [" + endpoint.URL + "] rejected the request with HTTP status [400]"
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}
			var transportErr *TransportError
			if !errors.As(err, &transportErr) {
				t.Fatalf("error = %v, want a transport rejection", err)
			}
			// Nothing was buffered for the caller: a truncated envelope is not
			// an answer.
			if queued, err := tr.Receive(context.Background()); err == nil {
				t.Fatalf("the truncated body was queued for the caller: %d bytes", len(queued))
			}
		})
	}
}

// TestDiscoveryRejectedOverAnEventStreamNegotiatesDown asserts a rejection the
// endpoint sends as an event stream steers the handshake exactly as the same
// rejection sent as a JSON body does. Reading it as a transport failure would
// send the client down the fallback instead, offering a version the server
// never named.
func TestDiscoveryRejectedOverAnEventStreamNegotiatesDown(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		switch request.method {
		case "server/discover":
			frame := jsonFrame(request.id, map[string]any{"error": map[string]any{
				"code":    CodeUnsupportedProtocolVersion,
				"message": "Unsupported protocol version",
				"data":    map[string]any{"supported": []any{ProtocolV20250618}},
			}})
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "event: message\ndata: "+frame+"\n\n")
		case "initialize":
			w.Header().Set(sessionHeader, "sess-http")
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"protocolVersion": ProtocolV20250618,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "Test Server", "version": "1.0.0"},
			}}))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})

	client := Web(endpoint.URL)
	version, err := client.ProtocolVersion(context.Background())
	if err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if version != ProtocolV20250618 {
		t.Fatalf("negotiated version = %q, want %q", version, ProtocolV20250618)
	}
	initialize := endpoint.request(t, "initialize")
	if !strings.Contains(initialize.body, `"protocolVersion":"`+ProtocolV20250618+`"`) {
		t.Fatalf("initialize body = %s, want it to offer the version the rejection named", initialize.body)
	}
}

// TestServerRequiringAClientCapability asserts the capability set a caller
// declares reaches a server that checks it. A server only sends the input
// requests of an unfinished result for a capability the client declared, so a
// client with no way to declare one could never be sent them.
func TestServerRequiringAClientCapability(t *testing.T) {
	newEndpoint := func(t *testing.T) *recordingEndpoint {
		return newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
			if !strings.Contains(request.body, `"io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}`) {
				writeJSON(w, http.StatusBadRequest, jsonFrame(request.id, map[string]any{"error": map[string]any{
					"code":    CodeMissingRequiredClientCapability,
					"message": "The [elicitation] capability is required.",
				}}))
				return
			}
			switch request.method {
			case "server/discover":
				writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
					"supportedVersions": []any{LatestProtocolVersion},
					"capabilities":      map[string]any{},
				}}))
			default:
				writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
					"resultType": "complete",
					"tools":      []any{},
				}}))
			}
		})
	}

	t.Run("a client declaring nothing is refused", func(t *testing.T) {
		endpoint := newEndpoint(t)
		_, err := Web(endpoint.URL).Tools(context.Background())

		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) {
			t.Fatalf("error = %v, want a JSON-RPC error", err)
		}
		if rpcErr.Code != CodeMissingRequiredClientCapability {
			t.Fatalf("error code = %d, want %d", rpcErr.Code, CodeMissingRequiredClientCapability)
		}
	})

	t.Run("a client declaring the capability is served", func(t *testing.T) {
		endpoint := newEndpoint(t)
		client := Web(endpoint.URL)
		client.WithClientCapabilities(map[string]any{"elicitation": map[string]any{}})

		if _, err := client.Tools(context.Background()); err != nil {
			t.Fatalf("tools: %v", err)
		}
		if got := endpoint.methods(); !slices.Equal(got, []string{"server/discover", "tools/list"}) {
			t.Fatalf("requests = %v", got)
		}
	})
}

// TestHeaderMismatchSentAsAnEventStreamIsRecovered asserts the recovery the
// specification asks for survives the form the rejection arrives in. A server
// answering a POST with an event stream may answer an unsuccessful one that way
// too, and the header mismatch it carries is the server on protocol terms: the
// client re-reads the definition and repeats the call with the header the
// refreshed one asks for. Read as a transport failure instead, the call would
// fail and take the connection with it.
func TestHeaderMismatchSentAsAnEventStreamIsRecovered(t *testing.T) {
	annotated := map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string", "x-mcp-header": "Region"},
			},
		},
	}
	plain := map[string]any{"name": "execute_sql", "inputSchema": map[string]any{"type": "object"}}

	var listings int
	var mu sync.Mutex
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		switch request.method {
		case "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{},
			}}))
		case "tools/list":
			// The first listing states the tool as it was; the annotation
			// arrives with the one the mismatch sends the client back for.
			mu.Lock()
			listings++
			tool := annotated
			if listings == 1 {
				tool = plain
			}
			mu.Unlock()
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"tools":      []any{tool},
			}}))
		default:
			if request.headers.Get("Mcp-Param-Region") == "" {
				frame := jsonFrame(request.id, map[string]any{"error": map[string]any{
					"code":    CodeHeaderMismatch,
					"message": "Header mismatch: The [Mcp-Param-Region] header is required.",
				}})
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "event: message\ndata: "+frame+"\n\n")
				return
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"content":    []any{map[string]any{"type": "text", "text": "done"}},
				"isError":    false,
			}}))
		}
	})

	client := Web(endpoint.URL)
	result, err := client.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}

	want := []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
	if got := endpoint.methods(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if got := endpoint.lastRequest(t, "tools/call").headers.Get("Mcp-Param-Region"); got != "us-west1" {
		t.Fatalf("the retry carried Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestConcurrentCallersShareOneCatalogue asserts the state a call by name reads
// its headers from stands up to being reached from several callers at once. The
// definitions a listing records, the tools it refused, and the capability set a
// caller declares are all written on one connection and read by every request
// that follows, so a listing running next to a call must leave the call with
// the header the definition asks for rather than with a torn view of it.
//
// The endpoint refuses a call that arrives without the header, which is what an
// intermediary routing on it does, and every call is held to what it carried on
// its own first attempt: a call recovering through the mismatch would otherwise
// hide a header the client failed to mirror.
func TestConcurrentCallersShareOneCatalogue(t *testing.T) {
	annotated := map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string", "x-mcp-header": "Region"},
				"query":  map[string]any{"type": "string"},
			},
		},
	}
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		switch request.method {
		case "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{},
				"ttlMs":             3600000,
				"cacheScope":        "private",
			}}))
		case "tools/list":
			// The catalogue states a lifetime, as the specification requires,
			// so what one caller's listing recorded is what the others read
			// rather than something each of them reads afresh.
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"tools":      []any{annotated},
				"ttlMs":      3600000,
				"cacheScope": "private",
			}}))
		default:
			if request.headers.Get("Mcp-Param-Region") != "us-west1" {
				writeJSON(w, http.StatusBadRequest, jsonFrame(request.id, map[string]any{"error": map[string]any{
					"code":    CodeHeaderMismatch,
					"message": "Header mismatch: The [Mcp-Param-Region] header is required.",
				}}))
				return
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"content":    []any{map[string]any{"type": "text", "text": "done"}},
				"isError":    false,
			}}))
		}
	})

	// Each caller drives one of the things that touch the shared state: a whole
	// listing, a capped one, a call declaring a capability along the way, and a
	// plain call.
	callers := []struct {
		name string
		run  func(client *WebClient) error
	}{
		{
			name: "a whole listing",
			run: func(client *WebClient) error {
				_, err := client.Tools(context.Background())
				return err
			},
		},
		{
			name: "a capped listing",
			run: func(client *WebClient) error {
				_, err := client.Tools(context.Background(), 1)
				return err
			},
		},
		{
			name: "a call declaring a capability",
			run: func(client *WebClient) error {
				client.WithClientCapabilities(map[string]any{"elicitation": map[string]any{}})
				_, err := client.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
				return err
			},
		},
		{
			name: "a call",
			run: func(client *WebClient) error {
				_, err := client.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
				return err
			},
		},
	}

	client := Web(endpoint.URL)
	var wg sync.WaitGroup
	failures := make(chan string, 4*len(callers))
	for range 4 {
		for _, caller := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := caller.run(client); err != nil {
					failures <- caller.name + ": " + err.Error()
				}
			}()
		}
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("%s", failure)
	}

	calls := endpoint.requestsFor("tools/call")
	if len(calls) != 8 {
		t.Fatalf("the endpoint received %d tools/call requests, want 8", len(calls))
	}
	for index, call := range calls {
		if got := call.headers.Get("Mcp-Param-Region"); got != "us-west1" {
			t.Fatalf("call %d carried Mcp-Param-Region = %q, want us-west1", index, got)
		}
		if !strings.Contains(call.body, `"region":"us-west1"`) {
			t.Fatalf("call %d body = %s, want it to state the mirrored argument too", index, call.body)
		}
	}
}

// indentJSON renders a compact JSON frame across several lines, which is how a
// server writing readable output states it.
func indentJSON(t *testing.T, frame string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(frame), "", "  "); err != nil {
		t.Fatalf("indent %s: %v", frame, err)
	}
	return out.String()
}

// sseDataLines renders a payload as the data fields of one event stream event,
// one field per line of the payload, without the blank line that ends the event.
func sseDataLines(payload string) string {
	var out strings.Builder
	for _, line := range strings.Split(payload, "\n") {
		out.WriteString("data: " + line + "\n")
	}
	return out.String()
}
