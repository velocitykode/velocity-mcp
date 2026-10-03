package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers what the client keeps of a request it withdrew. A server may
// answer a cancelled request with an error it puts no id on, which must not be
// taken for the answer to the next request; a server that conforms answers it
// with nothing, so what is kept must not wait for a reply for ever, grow
// without bound, or be kept at all where a late reply has nowhere to arrive.

// nullIDError is an error response a server could not put an id on.
const nullIDError = `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`

// oneStreamTransport is a transport that carries every exchange over one
// stream, as stdio does: a frame the server sends late is read by whoever reads
// next. It answers the handshake, never answers prompts/list, and answers
// tools/list with the frames it was given, the id placeholder among them
// rewritten to the id of that request.
type oneStreamTransport struct {
	mu sync.Mutex
	// answers is what the next tools/list is answered with.
	answers []string
	queue   []string
}

var (
	_ Transport         = (*oneStreamTransport)(nil)
	_ CancellationAware = (*oneStreamTransport)(nil)
)

func (o *oneStreamTransport) Connect(context.Context) error             { return nil }
func (o *oneStreamTransport) Disconnect() error                         { return nil }
func (o *oneStreamTransport) SetTimeout(time.Duration)                  {}
func (o *oneStreamTransport) Recipe() Recipe                            { return Recipe{Driver: "one-stream"} }
func (o *oneStreamTransport) NotifiesCancellation(ProtocolVersion) bool { return true }

func (o *oneStreamTransport) Send(_ context.Context, message string) error {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal([]byte(message), &frame)

	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case len(frame.ID) == 0, frame.Method == "prompts/list":
	case frame.Method == "server/discover":
		o.queue = append(o.queue, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"resultType":"complete",`+
			`"supportedVersions":["`+LatestProtocolVersion+`"],"capabilities":{},"ttlMs":600000}}`)
	default:
		for _, answer := range o.answers {
			o.queue = append(o.queue, strings.ReplaceAll(answer, scriptRequestID, string(frame.ID)))
		}
	}
	return nil
}

func (o *oneStreamTransport) Receive(ctx context.Context) (string, error) {
	o.mu.Lock()
	if len(o.queue) > 0 {
		frame := o.queue[0]
		o.queue = o.queue[1:]
		o.mu.Unlock()
		return frame, nil
	}
	o.mu.Unlock()

	<-ctx.Done()
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", wrapError(ctx.Err(), "the wait was cancelled")
	}
	return "", NewTimeoutError("timed out", ctx.Err())
}

// withdrawOne makes a request the server never answers and withdraws it.
func withdrawOne(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	defer cancel()
	if _, err := c.Prompts(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the withdrawn request = %v, want the caller's cancellation", err)
	}
}

// emptyTools is the answer to a tools/list, for the request in flight.
const emptyTools = `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":{"resultType":"complete","tools":[],"ttlMs":0}}`

// TestAWithdrawnRequestTakesOneLateErrorAndOnlyForAWhile asserts the rule for
// an error carrying a null id over a shared stream. Right after a request was
// withdrawn it is that request's reply and is passed over, once for each
// request withdrawn. Later, or once each withdrawn request has taken its one,
// it answers the request in flight, as it does when nothing was withdrawn: a
// server that conforms never answers a cancelled request, and its genuine
// errors must not be swallowed on the strength of a reply that will not come.
func TestAWithdrawnRequestTakesOneLateErrorAndOnlyForAWhile(t *testing.T) {
	tests := []struct {
		name string
		// withdrawn is how many requests are withdrawn first, and later how
		// long after the last of them the next request is made.
		withdrawn int
		later     time.Duration
		// answers is what the next request is answered with.
		answers []string
		// wantServerError is whether that request ends with the server's
		// null-id error; otherwise it is answered.
		wantServerError bool
	}{
		{name: "nothing withdrawn: the error answers the request", answers: []string{nullIDError}, wantServerError: true},
		{name: "one withdrawn: its late error is passed over", withdrawn: 1, answers: []string{nullIDError, emptyTools}},
		{name: "one withdrawn takes one error, the second answers the request", withdrawn: 1,
			answers: []string{nullIDError, nullIDError}, wantServerError: true},
		{name: "two withdrawn take two", withdrawn: 2, answers: []string{nullIDError, nullIDError, emptyTools}},
		{name: "long after the withdrawal the error answers the request", withdrawn: 1, later: time.Minute,
			answers: []string{nullIDError}, wantServerError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &oneStreamTransport{}
			c := New(transport, testClientInfo())
			var mu sync.Mutex
			now := time.Now()
			c.proto.now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			for range tt.withdrawn {
				withdrawOne(t, c)
			}
			mu.Lock()
			now = now.Add(tt.later)
			mu.Unlock()

			transport.mu.Lock()
			transport.answers = tt.answers
			transport.mu.Unlock()
			// The request has a deadline so that an error swallowed by mistake
			// ends the test with a timeout rather than holding it.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := c.Tools(ctx)

			var rpcErr *jsonrpc.Error
			if got := errors.As(err, &rpcErr); got != tt.wantServerError || (err != nil && !got) {
				t.Fatalf("error = %v, want the server's error = %v", err, tt.wantServerError)
			}
		})
	}
}

// TestWithdrawnRequestsAreNotKeptWithoutBound asserts a connection over which
// request after request is withdrawn, to a server that never answers one, does
// not keep them all.
func TestWithdrawnRequestsAreNotKeptWithoutBound(t *testing.T) {
	const withdrawn, limit = 80, 64
	c := New(&oneStreamTransport{}, testClientInfo())
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	for range withdrawn {
		withdrawOne(t, c)
	}
	c.proto.mu.Lock()
	kept := len(c.proto.unaccounted)
	c.proto.mu.Unlock()
	if kept > limit {
		t.Fatalf("%d withdrawn requests are kept, want at most %d", kept, limit)
	}
}

// TestAnHTTPErrorWithoutAnIDAnswersItsOwnRequest asserts nothing is kept over
// streamable HTTP, where the reply to a request arrives on the response to its
// own POST: after a request that timed out, the next request answered with an
// error carrying a null id is told that error, on either handshake.
func TestAnHTTPErrorWithoutAnIDAnswersItsOwnRequest(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "the revision without a session"
		if legacy {
			name = "a session revision"
		}
		t.Run(name, func(t *testing.T) {
			var endpoint *recordingEndpoint
			held := false
			endpoint = newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				switch {
				case request.httpMethod == http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				case request.method == "server/discover" && legacy:
					w.WriteHeader(http.StatusBadRequest)
				case request.method == "server/discover":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"resultType":        "complete",
						"supportedVersions": []any{LatestProtocolVersion},
						"capabilities":      map[string]any{},
						"ttlMs":             600000,
					}}))
				case request.method == "initialize":
					w.Header().Set(sessionHeader, "session-1")
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"protocolVersion": ProtocolV20251125,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "s", "version": "1"},
					}}))
				case request.method == "prompts/list" && !held:
					held = true
					serving := endpoint.current()
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, ": keep-alive\n\n")
					w.(http.Flusher).Flush()
					<-serving.Context().Done()
				case request.method == "prompts/list":
					writeJSON(w, http.StatusBadRequest, nullIDError)
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			})
			c := Web(endpoint.URL)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, err := c.Prompts(ctx)
			cancel()
			var timeoutErr *TimeoutError
			if !errors.As(err, &timeoutErr) {
				t.Fatalf("the first request = %v, want a timeout", err)
			}

			_, err = c.Prompts(context.Background())
			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != -32603 {
				t.Fatalf("the next request = %v, want the server's error", err)
			}
		})
	}
}
