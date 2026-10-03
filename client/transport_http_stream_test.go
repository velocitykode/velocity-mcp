package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file covers the event stream of a streamable HTTP reply as a stream: an
// event the server has flushed is delivered as it arrives, whether or not the
// server then ends the stream, and a stream that ends in a timeout or in the
// caller withdrawing its request is reported as what it is rather than as no
// message having arrived. The watchdogs below are ceilings on a client that
// never returns, not waits the tests rely on.

// watchdog is how long a test waits for a call that must return at once before
// reporting it stuck.
const watchdog = 5 * time.Second

// streamingEndpoint answers the discovery handshake and a tools/list, and hands
// every tools/call to reply, which writes the response stream.
func streamingEndpoint(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, request recordedRequest)) *recordingEndpoint {
	t.Helper()
	var endpoint *recordingEndpoint
	endpoint = newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		switch request.method {
		case "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType":        "complete",
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{"tools": map[string]any{}},
				"ttlMs":             600000,
			}}))
		case "tools/list":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"tools":      []any{plainTool("echo")},
				"ttlMs":      600000,
			}}))
		case "tools/call":
			reply(w, endpoint.current(), request)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	return endpoint
}

// current returns the request being served, which the handler stores so a reply
// can wait on the request's context.
func (e *recordingEndpoint) current() *http.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.serving
}

// TestAFlushedResponseIsDeliveredWhileTheStreamStaysOpen asserts the response
// the server flushed on the event stream reaches the caller at once, although
// the server leaves the stream open afterwards: ending it is only a SHOULD of
// the specification, and a client waiting for the end would hold every answer
// until its own timeout.
func TestAFlushedResponseIsDeliveredWhileTheStreamStaysOpen(t *testing.T) {
	tests := []struct {
		name string
		// after is what the server does with the stream once the response is
		// flushed; release is closed when the test is over.
		after func(r *http.Request, release <-chan struct{})
	}{
		{
			name: "the server leaves the stream open",
			after: func(r *http.Request, release <-chan struct{}) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			},
		},
		{
			name:  "the server ends the stream",
			after: func(*http.Request, <-chan struct{}) {},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			endpoint := streamingEndpoint(t, func(w http.ResponseWriter, r *http.Request, request recordedRequest) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+jsonFrame(request.id, map[string]any{"result": map[string]any{
					"resultType": "complete",
					"content":    []any{map[string]any{"type": "text", "text": "done"}},
				}})+"\n\n")
				w.(http.Flusher).Flush()
				tc.after(r, release)
			})
			// Registered after the endpoint's own cleanup, so it runs before it:
			// the server's Close waits for the handler, and the handler waits
			// for this.
			t.Cleanup(func() { close(release) })
			c := Web(endpoint.URL)

			type outcome struct {
				result *ToolResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := c.CallTool(context.Background(), "echo", nil)
				done <- outcome{result, err}
			}()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("call: %v", got.err)
				}
				if got.result.Text() != "done" {
					t.Fatalf("text = %q, want done", got.result.Text())
				}
			case <-time.After(watchdog):
				t.Fatal("the call did not return once the server had flushed the response")
			}
		})
	}
}

// TestAnHTTPTimeoutIsATypedTimeout asserts the transport's own timeout is
// reported as a TimeoutError, which is also a TransportError, whether it passes
// while waiting for the response headers or in the middle of an event stream,
// so callers can tell it from a protocol fault as they can over stdio.
func TestAnHTTPTimeoutIsATypedTimeout(t *testing.T) {
	tests := []struct {
		name  string
		reply func(w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "while waiting for the headers",
			reply: func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
		},
		{
			name: "in the middle of the stream",
			reply: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, ": keep-alive\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := streamingEndpoint(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
				tc.reply(w, r)
			})
			c := Web(endpoint.URL)
			c.WithTimeout(200 * time.Millisecond)

			_, err := c.CallTool(context.Background(), "echo", nil)
			var timeoutErr *TimeoutError
			if !errors.As(err, &timeoutErr) {
				t.Fatalf("error = %v (%T), want a timeout", err, err)
			}
			var transportErr *TransportError
			if !errors.As(err, &transportErr) {
				t.Fatalf("error = %v, want it to be a transport failure as well", err)
			}
			want := "timed out while waiting for a response from [" + endpoint.URL + "]"
			if !strings.HasPrefix(err.Error(), want+": ") {
				t.Fatalf("error = %q, want it to start with %q", err.Error(), want)
			}
		})
	}
}

// TestACallerDeadlineDuringAStreamIsATimeout asserts the caller's own deadline
// passing in the middle of a stream is a timeout too, carrying the deadline in
// its chain rather than surfacing as no message having arrived.
func TestACallerDeadlineDuringAStreamIsATimeout(t *testing.T) {
	tests := []struct {
		name  string
		reply func(w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "in the middle of the stream",
			reply: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, ": keep-alive\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			},
		},
		{
			name:  "while waiting for the headers",
			reply: func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := streamingEndpoint(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
				tc.reply(w, r)
			})
			c := Web(endpoint.URL)
			if _, err := c.Tools(context.Background()); err != nil {
				t.Fatalf("tools: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := c.CallTool(ctx, "echo", nil)
			var timeoutErr *TimeoutError
			if !errors.As(err, &timeoutErr) {
				t.Fatalf("error = %v (%T), want a timeout", err, err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want it to carry the caller's deadline", err)
			}
		})
	}
}

// TestACallerWithdrawingDuringAStreamIsNotATimeout asserts a caller cancelling
// its context in the middle of a stream is reported as the withdrawal it is:
// neither a timeout nor a failure of the channel, carrying the cancellation in
// its chain. The stream it closes is the cancellation signal of streamable
// HTTP, so nothing else is sent.
func TestACallerWithdrawingDuringAStreamIsNotATimeout(t *testing.T) {
	tests := []struct {
		name string
		// reply holds the call; it calls reached once the call is where the
		// case wants it withdrawn.
		reply func(w http.ResponseWriter, r *http.Request, reached func())
	}{
		{
			name: "in the middle of the stream",
			reply: func(w http.ResponseWriter, r *http.Request, reached func()) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, ": keep-alive\n\n")
				w.(http.Flusher).Flush()
				reached()
				<-r.Context().Done()
			},
		},
		{
			name: "while waiting for the headers",
			reply: func(_ http.ResponseWriter, r *http.Request, reached func()) {
				reached()
				<-r.Context().Done()
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			held := make(chan struct{})
			endpoint := streamingEndpoint(t, func(w http.ResponseWriter, r *http.Request, _ recordedRequest) {
				tc.reply(w, r, func() { close(held) })
			})
			c := Web(endpoint.URL)
			if _, err := c.Tools(context.Background()); err != nil {
				t.Fatalf("tools: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.CallTool(ctx, "echo", nil)
				done <- err
			}()
			select {
			case <-held:
			case <-time.After(watchdog):
				t.Fatal("the server never got the call")
			}
			cancel()

			var err error
			select {
			case err = <-done:
			case <-time.After(watchdog):
				t.Fatal("the call did not return once its context was cancelled")
			}
			if err == nil {
				t.Fatal("a withdrawn call was reported as answered")
			}
			var timeoutErr *TimeoutError
			if errors.As(err, &timeoutErr) {
				t.Fatalf("error = %v, want a cancellation rather than a timeout", err)
			}
			var transportErr *TransportError
			if errors.As(err, &transportErr) {
				t.Fatalf("error = %v, want a cancellation rather than a transport failure", err)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want it to carry the cancellation", err)
			}
			want := "the wait for a response from [" + endpoint.URL + "] was cancelled"
			if !strings.HasPrefix(err.Error(), want+": ") {
				t.Fatalf("error = %q, want it to start with %q", err.Error(), want)
			}
			if posted := endpoint.requestsFor("notifications/cancelled"); len(posted) != 0 {
				t.Fatalf("notifications/cancelled was POSTed %d time(s); closing the stream is the signal", len(posted))
			}
		})
	}
}

// TestARejectionFlushedOnAStreamIsReadWithoutWaitingForItsEnd asserts the
// same for an unsuccessful status answered with an event stream: the rejection
// the server flushed is the answer, and a server that then leaves the stream
// open holds nothing up. The probe is negotiated down on the rejection while
// the stream still stands.
func TestARejectionFlushedOnAStreamIsReadWithoutWaitingForItsEnd(t *testing.T) {
	tests := []struct {
		name string
		// rejection is the error member the probe is answered with.
		rejection map[string]any
	}{
		{
			name: "a rejection that names the versions the server supports",
			rejection: map[string]any{
				"code":    CodeUnsupportedProtocolVersion,
				"message": "Unsupported protocol version",
				"data":    map[string]any{"supported": []string{ProtocolV20251125}},
			},
		},
		{
			name:      "a rejection from a server that does not know the probe",
			rejection: map[string]any{"code": -32601, "message": "Method not found."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				switch request.method {
				case "server/discover":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, "data: "+jsonFrame(request.id, map[string]any{"error": tc.rejection})+"\n\n")
					w.(http.Flusher).Flush()
					<-release
				case "initialize":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"protocolVersion": ProtocolV20251125,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "s", "version": "1"},
					}}))
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			})
			// Registered after the endpoint's own cleanup, so it runs before it.
			t.Cleanup(func() { close(release) })
			c := Web(endpoint.URL)

			type outcome struct {
				version ProtocolVersion
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				version, err := c.ProtocolVersion(context.Background())
				done <- outcome{version, err}
			}()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("connect: %v", got.err)
				}
				if got.version != ProtocolV20251125 {
					t.Fatalf("version = %q, want %q", got.version, ProtocolV20251125)
				}
			case <-time.After(watchdog):
				t.Fatal("the probe did not settle while the server held the rejection's stream open")
			}
		})
	}
}

// TestARequestAnsweredWithNothingIsNotReadFromTheStreamBefore asserts a request
// the server answers with an empty reply is reported as that. The exchange
// before it was answered on an event stream the server left open; that stream
// belongs to an exchange that is over, and reading it for the reply to the next
// request would report how the earlier one ended, a cancellation nobody made,
// in place of what happened: the server sent no message.
func TestARequestAnsweredWithNothingIsNotReadFromTheStreamBefore(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "an empty body", status: http.StatusOK},
		{name: "an accepted request", status: http.StatusAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			endpoint := streamingEndpoint(t, func(w http.ResponseWriter, r *http.Request, request recordedRequest) {
				calls++
				if calls > 1 {
					w.WriteHeader(tt.status)
					return
				}
				// The first call is answered on a stream the server leaves open.
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+jsonFrame(request.id, map[string]any{"result": map[string]any{
					"resultType": "complete",
					"content":    []any{map[string]any{"type": "text", "text": "done"}},
					"isError":    false,
				}})+"\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			c := Web(endpoint.URL)
			if _, err := c.CallTool(context.Background(), "echo", nil); err != nil {
				t.Fatalf("the call answered on the stream: %v", err)
			}

			_, err := c.CallTool(context.Background(), "echo", nil)
			if err == nil {
				t.Fatal("a call the server answered with nothing reported success")
			}
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "cancelled") {
				t.Fatalf("error = %q, want what happened rather than a cancellation nobody made", err.Error())
			}
			if err.Error() != "no message available from the HTTP transport" {
				t.Fatalf("error = %q, want the empty reply reported", err.Error())
			}
		})
	}
}
