package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/schema"
)

// This file covers the bound on one exchange. The MCP specification (2026-07-28,
// basic/utilities/progress and basic/lifecycle#timeouts) has an implementation
// enforce a maximum timeout whatever progress notifications arrive, so a server
// that keeps sending frames that are not the response cannot keep a caller, and
// everyone queued behind it, waiting.

// boundTimeout is the timeout the tests below set, and boundCeiling how long
// they give an exchange to end before calling it unbounded.
const (
	boundTimeout = 50 * time.Millisecond
	boundCeiling = 3 * time.Second
)

// floodingTransport answers the handshake and then, for every other request,
// delivers notifications for as long as it is read. It never looks at the
// context of a read, which is what a transport written outside this package may
// do: the bound has to hold over it all the same.
type floodingTransport struct {
	mu        sync.Mutex
	queue     []string
	flooding  bool
	delivered atomic.Int64
	// stated is the timeout the transport states, when it states one.
	stated time.Duration
}

var _ Transport = (*floodingTransport)(nil)

func (f *floodingTransport) Connect(context.Context) error { return nil }
func (f *floodingTransport) Disconnect() error             { return nil }
func (f *floodingTransport) SetTimeout(time.Duration)      {}
func (f *floodingTransport) Recipe() Recipe                { return Recipe{Driver: "flooding"} }

func (f *floodingTransport) Send(_ context.Context, message string) error {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal([]byte(message), &frame)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(frame.ID) == 0:
	case frame.Method == "server/discover":
		f.queue = append(f.queue, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"resultType":"complete",`+
			`"supportedVersions":["`+LatestProtocolVersion+`"],"capabilities":{},"ttlMs":600000}}`)
	default:
		f.flooding = true
	}
	return nil
}

func (f *floodingTransport) Receive(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) > 0 {
		frame := f.queue[0]
		f.queue = f.queue[1:]
		return frame, nil
	}
	if !f.flooding {
		return "", newError("flooding transport: no message queued")
	}
	f.delivered.Add(1)
	return `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"not-this-request","progress":1}}`, nil
}

// statingTransport is a flooding transport that states its timeout, as the
// transports of this package do.
type statingTransport struct{ *floodingTransport }

func (s statingTransport) Timeout() time.Duration { return s.stated }

// TestAStreamOfNotificationsDoesNotOutlastTheTimeout asserts the bound: an
// exchange a server keeps feeding notifications ends when the timeout does, with
// the error the way it ended calls for, and the next caller finds the gate free.
func TestAStreamOfNotificationsDoesNotOutlastTheTimeout(t *testing.T) {
	tests := []struct {
		name string
		// build returns the client under test over a flooding transport.
		build func(*floodingTransport) *Client
		// ctx returns the context of the call and what releases it.
		ctx         func() (context.Context, context.CancelFunc)
		wantTimeout bool
		wantCause   error
	}{
		{
			name: "the client timeout bounds a call whose context has no deadline",
			build: func(f *floodingTransport) *Client {
				return New(f, schema.Implementation{}).WithTimeout(boundTimeout)
			},
			ctx:         func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantTimeout: true,
			wantCause:   context.DeadlineExceeded,
		},
		{
			name: "the timeout a transport states bounds the call",
			build: func(f *floodingTransport) *Client {
				f.stated = boundTimeout
				return New(statingTransport{f}, schema.Implementation{})
			},
			ctx:         func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantTimeout: true,
			wantCause:   context.DeadlineExceeded,
		},
		{
			name: "the caller's own deadline bounds the call",
			build: func(f *floodingTransport) *Client {
				return New(f, schema.Implementation{})
			},
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), boundTimeout)
			},
			wantTimeout: true,
			wantCause:   context.DeadlineExceeded,
		},
		{
			name: "a caller withdrawing the call ends it, and not as a timeout",
			build: func(f *floodingTransport) *Client {
				return New(f, schema.Implementation{})
			},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(boundTimeout, cancel)
				return ctx, cancel
			},
			wantTimeout: false,
			wantCause:   context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &floodingTransport{}
			c := tt.build(transport)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			ctx, cancel := tt.ctx()
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.Prompts(ctx)
				done <- err
			}()

			var err error
			select {
			case err = <-done:
			case <-time.After(boundCeiling):
				// The call is released so the test ends, and reported for what
				// it was: still running long after its bound.
				cancel()
				t.Fatalf("the exchange was still running after %v with a bound of %v: %d notifications read",
					boundCeiling, boundTimeout, transport.delivered.Load())
			}

			if err == nil {
				t.Fatal("an exchange that was never answered reported success")
			}
			var timeoutErr *TimeoutError
			if got := errors.As(err, &timeoutErr); got != tt.wantTimeout {
				t.Fatalf("error = %v (%T), timeout = %v, want %v", err, err, got, tt.wantTimeout)
			}
			if !errors.Is(err, tt.wantCause) {
				t.Fatalf("error = %v, want %v in its chain", err, tt.wantCause)
			}

			// The gate is free again: a second caller is not parked behind the
			// exchange that ended.
			second, release := context.WithCancel(context.Background())
			release()
			if err := c.Ping(second); !errors.Is(err, context.Canceled) ||
				err.Error() != "the request was abandoned before it was sent: context canceled" {
				t.Fatalf("second caller = %v, want it turned away at a free gate", err)
			}
		})
	}
}

// TestAStdioNotificationFloodEndsWithTheTimeout is the bound over the real stdio
// transport, whose wait for one frame starts over with every frame it reads.
func TestAStdioNotificationFloodEndsWithTheTimeout(t *testing.T) {
	script := `while IFS= read -r line; do case "$line" in ` +
		`*server/discover*) echo '{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete",` +
		`"supportedVersions":["` + LatestProtocolVersion + `"],"capabilities":{},"ttlMs":600000}}';; ` +
		`*prompts/list*) while :; do echo '{"jsonrpc":"2.0","method":"notifications/progress",` +
		`"params":{"progressToken":"not-this-request","progress":1}}' || exit 0; done;; esac; done`
	transport := NewStdioTransport("/bin/sh", "-c", script)
	c := New(transport, schema.Implementation{})
	defer c.Disconnect()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// The handshake had the default timeout, which a slow machine may need to
	// start the subprocess; only the flooded call is held to the short one.
	c.WithTimeout(200 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Prompts(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		var timeoutErr *TimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("error = %v (%T), want a timeout", err, err)
		}
	case <-time.After(boundCeiling):
		cancel()
		t.Fatalf("the exchange was still running after %v with a timeout of 200ms", boundCeiling)
	}
}

// progressingTransport answers the handshake and then, for every other request,
// reports progress at an interval before it answers: a server that is working
// on the request and saying so. Under token "own" the progress it reports names
// the progress token the request carried; under any other it names that token
// instead, which is progress on something else. It honours the context of a
// read, as the transports of this package do.
type progressingTransport struct {
	interval time.Duration
	// reports is how many times progress is reported before the answer; a
	// negative number reports it for ever and never answers.
	reports int
	token   string

	mu      sync.Mutex
	queue   []string
	pending string // id of the request being worked on
	asked   string // progress token it carried
	sent    int
}

var _ Transport = (*progressingTransport)(nil)

func (p *progressingTransport) Connect(context.Context) error { return nil }
func (p *progressingTransport) Disconnect() error             { return nil }
func (p *progressingTransport) SetTimeout(time.Duration)      {}
func (p *progressingTransport) Recipe() Recipe                { return Recipe{Driver: "progressing"} }

func (p *progressingTransport) Send(_ context.Context, message string) error {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Meta struct {
				Token json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		} `json:"params"`
	}
	_ = json.Unmarshal([]byte(message), &frame)

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case len(frame.ID) == 0:
	case frame.Method == "server/discover":
		p.queue = append(p.queue, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"resultType":"complete",`+
			`"supportedVersions":["`+LatestProtocolVersion+`"],"capabilities":{},"ttlMs":600000}}`)
	default:
		p.pending, p.asked, p.sent = string(frame.ID), string(frame.Params.Meta.Token), 0
	}
	return nil
}

func (p *progressingTransport) Receive(ctx context.Context) (string, error) {
	p.mu.Lock()
	if len(p.queue) > 0 {
		frame := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		return frame, nil
	}
	pending, asked, done := p.pending, p.asked, p.reports >= 0 && p.sent >= p.reports
	p.sent++
	p.mu.Unlock()
	if pending == "" {
		return "", newError("progressing transport: no request is being worked on")
	}

	timer := time.NewTimer(p.interval)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", wrapError(ctx.Err(), "the wait was cancelled")
		}
		return "", NewTimeoutError("timed out", ctx.Err())
	}
	if done {
		return `{"jsonrpc":"2.0","id":` + pending + `,"result":{"resultType":"complete",` +
			`"content":[{"type":"text","text":"done"}],"isError":false}}`, nil
	}
	token := asked
	if p.token != "own" {
		token = p.token
	}
	if token == "" {
		// The request asked for no progress; what is reported names nothing.
		token = `"unasked"`
	}
	return `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":` + token + `,"progress":1}}`, nil
}

// TestProgressOnARequestStartsItsTimeoutOver asserts the timeouts the MCP
// specification describes for a request (basic/lifecycle#timeouts): progress
// reported for the request starts its timeout over, so work that is under way
// and says so is not cut; progress on anything else does not; and a maximum is
// enforced whatever is reported, so a server cannot hold a caller for good by
// reporting progress. A deadline on the caller's context is the bound in place
// of both.
func TestProgressOnARequestStartsItsTimeoutOver(t *testing.T) {
	const timeout = 200 * time.Millisecond
	tests := []struct {
		name      string
		transport *progressingTransport
		// maximum is the maximum set on the client, or zero for the default.
		maximum time.Duration
		// deadline is the deadline of the caller's context, or zero for none.
		deadline time.Duration
		// wantAnswered is whether the request is answered; otherwise it times
		// out, no sooner than atLeast and no later than atMost.
		wantAnswered    bool
		atLeast, atMost time.Duration
	}{
		{
			name:         "progress on the request keeps it alive past the timeout",
			transport:    &progressingTransport{interval: timeout / 4, reports: 12, token: "own"},
			wantAnswered: true,
			atLeast:      3 * timeout,
		},
		{
			name:      "progress on another request does not",
			transport: &progressingTransport{interval: timeout / 4, reports: 12, token: `"another"`},
			atLeast:   timeout - timeout/4, atMost: 2 * timeout,
		},
		{
			name:      "a token of another JSON type is another token",
			transport: &progressingTransport{interval: timeout / 4, reports: 12, token: `"1"`},
			atLeast:   timeout - timeout/4, atMost: 2 * timeout,
		},
		{
			name:      "progress for ever ends at the maximum",
			transport: &progressingTransport{interval: timeout / 4, reports: -1, token: "own"},
			maximum:   3 * timeout,
			atLeast:   3*timeout - timeout/4, atMost: 5 * timeout,
		},
		{
			name:      "the maximum defaults to ten times the timeout",
			transport: &progressingTransport{interval: timeout / 4, reports: -1, token: "own"},
			atLeast:   10*timeout - timeout/4, atMost: 14 * timeout,
		},
		{
			name:      "silence ends at the timeout",
			transport: &progressingTransport{interval: time.Hour, reports: -1, token: "own"},
			atLeast:   timeout - timeout/4, atMost: 2 * timeout,
		},
		{
			name:      "the caller's deadline is the bound in place of the timeout",
			transport: &progressingTransport{interval: time.Hour, reports: -1, token: "own"},
			deadline:  3 * timeout,
			atLeast:   3*timeout - timeout/4, atMost: 5 * timeout,
		},
		{
			name:      "the caller's deadline is the bound in place of the maximum",
			transport: &progressingTransport{interval: timeout / 4, reports: -1, token: "own"},
			maximum:   2 * timeout,
			deadline:  4 * timeout,
			atLeast:   4*timeout - timeout/4, atMost: 6 * timeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := New(tt.transport, schema.Implementation{}).WithTimeout(timeout)
			if tt.maximum > 0 {
				c.WithMaxTimeout(tt.maximum)
			}
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			if tt.deadline > 0 {
				ctx, cancel = context.WithTimeout(context.Background(), tt.deadline)
			}
			defer cancel()
			// The call is given a ceiling of its own, well past the bound under
			// test: a call nothing ends is withdrawn so the test can say so.
			ceiling := tt.atMost + 3*time.Second
			if tt.wantAnswered {
				ceiling = tt.atLeast + 5*time.Second
			}
			type outcome struct {
				result *ToolResult
				err    error
			}
			done := make(chan outcome, 1)
			start := time.Now()
			go func() {
				result, err := c.CallTool(ctx, "slow", nil)
				done <- outcome{result, err}
			}()
			var result *ToolResult
			var err error
			select {
			case got := <-done:
				result, err = got.result, got.err
			case <-time.After(ceiling):
				cancel()
				<-done
				t.Fatalf("the request was still running after %v; nothing bounded it", ceiling)
			}
			took := time.Since(start)

			if took < tt.atLeast {
				t.Fatalf("the request ended after %v, want no sooner than %v (err = %v)", took, tt.atLeast, err)
			}
			if tt.wantAnswered {
				if err != nil {
					t.Fatalf("a request the server was working on, and said so, ended after %v: %v", took, err)
				}
				if result.Text() != "done" {
					t.Fatalf("text = %q, want done", result.Text())
				}
				return
			}
			var timeoutErr *TimeoutError
			if !errors.As(err, &timeoutErr) {
				t.Fatalf("error = %v (%T) after %v, want a timeout", err, err, took)
			}
			if took > tt.atMost {
				t.Fatalf("the request ended after %v, want no later than %v", took, tt.atMost)
			}
		})
	}
}

// TestAStdioCallReportingProgressOutlivesTheTimeout is the same over the real
// stdio transport and a real subprocess: a tool call that takes a second, and
// reports progress every tenth of one, completes under a timeout of three
// tenths.
func TestAStdioCallReportingProgressOutlivesTheTimeout(t *testing.T) {
	t.Setenv(cancellableServerEnv, "1")
	t.Setenv(cancellableLogEnv, filepath.Join(t.TempDir(), "server.log"))
	transport := NewStdioTransport(os.Args[0])
	transport.SetTimeout(30 * time.Second)
	c := New(transport, testClientInfo())
	defer c.Disconnect()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	c.WithTimeout(300 * time.Millisecond)

	start := time.Now()
	result, err := c.CallTool(context.Background(), "slow", nil)
	if err != nil {
		t.Fatalf("a call the server was reporting progress on ended after %v: %v", time.Since(start), err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	if !c.Connected() {
		t.Fatal("the call took the connection down")
	}
}
