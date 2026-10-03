package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file covers a request the client stops waiting for, over the transport
// the rule is written for. The MCP specification (2026-07-28,
// basic/utilities/cancellation) has a client cancel a request over stdio by
// sending notifications/cancelled naming it, and a request that timed out the
// same way: the server is one process for every request of the connection, and
// ending it ends all of them.

// cancellableServerEnv marks a process as the scripted server below rather than
// the test binary, cancellableLogEnv names the file it records every frame it
// reads in, and cancellableLateEnv has it answer a request after it was told
// the request is cancelled.
const (
	cancellableServerEnv = "VELOCITY_MCP_TEST_CANCELLABLE_STDIO_SERVER"
	cancellableLogEnv    = "VELOCITY_MCP_TEST_CANCELLABLE_STDIO_LOG"
	cancellableLateEnv   = "VELOCITY_MCP_TEST_CANCELLABLE_STDIO_LATE"
)

// runCancellableStdioServer speaks the discovery revision over stdio. It
// answers server/discover and tools/list, takes a second over a tools/call while
// reporting progress on it, and never answers prompts/list by itself: that is
// the request the tests abandon. It records "started" once and
// then every frame it reads, so a test can tell what reached it and whether it
// is still the process that was started. It returns the process exit status.
func runCancellableStdioServer() int {
	log, err := os.OpenFile(os.Getenv(cancellableLogEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log: %v\n", err)
		return 2
	}
	defer log.Close()
	fmt.Fprintln(log, "started")

	late := os.Getenv(cancellableLateEnv)
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			fmt.Fprintln(log, strings.TrimSpace(line))
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					RequestID json.RawMessage `json:"requestId"`
					Meta      struct {
						Token json.RawMessage `json:"progressToken"`
					} `json:"_meta"`
				} `json:"params"`
			}
			if decodeErr := json.Unmarshal([]byte(line), &frame); decodeErr != nil {
				fmt.Fprintf(os.Stderr, "unreadable frame: %v\n", decodeErr)
				return 2
			}
			switch frame.Method {
			case "server/discover":
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete",`+
					`"supportedVersions":[%q],"capabilities":{"tools":{},"prompts":{}},"ttlMs":600000}}`+"\n",
					frame.ID, LatestProtocolVersion)
			case "tools/list":
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","tools":[],"ttlMs":0}}`+"\n", frame.ID)
			case "tools/call":
				// A call that takes a second, during which the server reports
				// progress under the token the request asked for it under.
				token := string(frame.Params.Meta.Token)
				if token == "" {
					// The request asked for no progress; what is reported
					// names nothing.
					token = `"unasked"`
				}
				for step := 1; step <= 10; step++ {
					time.Sleep(100 * time.Millisecond)
					fmt.Printf(`{"jsonrpc":"2.0","method":"notifications/progress",`+
						`"params":{"progressToken":%s,"progress":%d,"total":10}}`+"\n", token, step)
				}
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete",`+
					`"content":[{"type":"text","text":"done"}],"isError":false}}`+"\n", frame.ID)
			case "notifications/cancelled":
				switch late {
				case "1":
					// A server may already have the response on its way when the
					// cancellation reaches it.
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete",`+
						`"prompts":[{"name":"too-late"}],"ttlMs":0}}`+"\n", frame.Params.RequestID)
				case "null":
					// Or it answers the cancelled request with an error it puts
					// no id on.
					fmt.Println(`{"jsonrpc":"2.0","id":null,"error":{"code":-32800,"message":"request cancelled"}}`)
				}
			}
		}
		if err != nil {
			return 0
		}
	}
}

// loggedFrame is one frame the scripted server recorded.
type loggedFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
		Meta      map[string]any  `json:"_meta"`
	} `json:"params"`
}

// readServerLog returns how many times the scripted server started and the
// frames it read, in order.
func readServerLog(t *testing.T, path string) (starts int, frames []loggedFrame) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "started" {
			starts++
			continue
		}
		var frame loggedFrame
		if err := json.Unmarshal([]byte(line), &frame); err == nil {
			frames = append(frames, frame)
		}
	}
	return starts, frames
}

// framesOf returns the recorded frames of one method.
func framesOf(frames []loggedFrame, method string) []loggedFrame {
	var out []loggedFrame
	for _, frame := range frames {
		if frame.Method == method {
			out = append(out, frame)
		}
	}
	return out
}

// TestAnAbandonedStdioRequestIsCancelledAndTheServerKept asserts what a request
// the client stops waiting for costs over stdio: one notifications/cancelled
// frame naming it, and nothing else. The server is the same process afterwards,
// the connection is the one that was negotiated, and the next request is
// answered over it, whether or not the server answered the abandoned one late.
func TestAnAbandonedStdioRequestIsCancelledAndTheServerKept(t *testing.T) {
	const short = 300 * time.Millisecond
	tests := []struct {
		name string
		// abandon makes the call under test and returns its error.
		abandon func(c *Client) error
		// late is how the server answers the request once it is cancelled, if
		// it does: "1" with its result, "null" with an error carrying no id.
		late        string
		wantTimeout bool
		wantCause   error
		wantReason  string
	}{
		{
			name: "the caller withdraws the request",
			abandon: func(c *Client) error {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(short, cancel)
				_, err := c.Prompts(ctx)
				return err
			},
			wantCause:  context.Canceled,
			wantReason: "the caller withdrew the request",
		},
		{
			name: "the client timeout passes",
			abandon: func(c *Client) error {
				c.WithTimeout(short)
				defer c.WithTimeout(30 * time.Second)
				_, err := c.Prompts(context.Background())
				return err
			},
			wantTimeout: true,
			wantReason:  "the request timed out",
		},
		{
			name: "the caller's deadline passes",
			abandon: func(c *Client) error {
				ctx, cancel := context.WithTimeout(context.Background(), short)
				defer cancel()
				_, err := c.Prompts(ctx)
				return err
			},
			wantTimeout: true,
			wantCause:   context.DeadlineExceeded,
			wantReason:  "the request timed out",
		},
		{
			name: "the server answers the request after it was cancelled",
			abandon: func(c *Client) error {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(short, cancel)
				_, err := c.Prompts(ctx)
				return err
			},
			late:       "1",
			wantCause:  context.Canceled,
			wantReason: "the caller withdrew the request",
		},
		{
			name: "the server answers the cancelled request with an error carrying a null id",
			abandon: func(c *Client) error {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(short, cancel)
				_, err := c.Prompts(ctx)
				return err
			},
			late:       "null",
			wantCause:  context.Canceled,
			wantReason: "the caller withdrew the request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "server.log")
			t.Setenv(cancellableServerEnv, "1")
			t.Setenv(cancellableLogEnv, log)
			if tt.late != "" {
				t.Setenv(cancellableLateEnv, tt.late)
			}
			transport := NewStdioTransport(os.Args[0])
			transport.SetTimeout(30 * time.Second)
			c := New(transport, testClientInfo())
			defer c.Disconnect()
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			err := tt.abandon(c)
			if err == nil {
				t.Fatal("a request that was never answered reported success")
			}
			var timeoutErr *TimeoutError
			if got := errors.As(err, &timeoutErr); got != tt.wantTimeout {
				t.Fatalf("error = %v (%T), timeout = %v, want %v", err, err, got, tt.wantTimeout)
			}
			if tt.wantCause != nil && !errors.Is(err, tt.wantCause) {
				t.Fatalf("error = %v, want %v in its chain", err, tt.wantCause)
			}

			// The cancellation is on the wire by the time the call returns; the
			// server records it a moment later.
			var cancelled []loggedFrame
			if !pollFor(10*time.Second, func() bool {
				_, frames := readServerLog(t, log)
				cancelled = framesOf(frames, "notifications/cancelled")
				return len(cancelled) > 0
			}) {
				starts, frames := readServerLog(t, log)
				t.Fatalf("the server was never told the request was cancelled; it started %d time(s) and read %d frame(s)",
					starts, len(frames))
			}
			_, frames := readServerLog(t, log)
			abandoned := framesOf(frames, "prompts/list")
			if len(abandoned) != 1 || len(cancelled) != 1 {
				t.Fatalf("the server read %d prompts/list and %d cancellation frame(s), want one of each",
					len(abandoned), len(cancelled))
			}
			notice := cancelled[0]
			if len(notice.ID) != 0 {
				t.Fatalf("the cancellation carries the id %s; it is a notification", notice.ID)
			}
			if string(notice.Params.RequestID) != string(abandoned[0].ID) {
				t.Fatalf("the cancellation names request %s, want %s", notice.Params.RequestID, abandoned[0].ID)
			}
			if notice.Params.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", notice.Params.Reason, tt.wantReason)
			}
			if got := notice.Params.Meta[MetaProtocolVersion]; got != LatestProtocolVersion {
				t.Fatalf("the cancellation states protocol version %v, want %q", got, LatestProtocolVersion)
			}

			// The connection is the one that was negotiated, and the next
			// request is answered over it by the same process.
			if !c.Connected() {
				t.Fatal("the abandoned request took the connection down")
			}
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("the request after the abandoned one: %v", err)
			}
			if len(tools) != 0 {
				t.Fatalf("tools = %v, want the empty catalogue of the server", tools)
			}
			starts, frames := readServerLog(t, log)
			if starts != 1 {
				t.Fatalf("the server was started %d time(s), want the one process kept", starts)
			}
			if got := len(framesOf(frames, "server/discover")); got != 1 {
				t.Fatalf("the connection was negotiated %d time(s), want once", got)
			}
		})
	}
}

// TestAServerSilentThroughTwoTimeoutsIsReplaced asserts the limit of keeping a
// server through a request that timed out. One timeout is one slow request, and
// the server is kept and told the request is cancelled. A second timeout with
// not one frame from the server since the first is a server that has stopped:
// it is given up, and the next request starts a new one. A server that answers
// anything in between is not silent, and is kept however many of its requests
// time out.
func TestAServerSilentThroughTwoTimeoutsIsReplaced(t *testing.T) {
	const short = 300 * time.Millisecond
	// timeOut makes a request the server never answers and requires it to time
	// out; answered makes one it does answer. Each connects first, under no
	// deadline, so a server being started is not what the short deadline meets.
	timeOut := func(t *testing.T, c *Client) {
		t.Helper()
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), short)
		defer cancel()
		_, err := c.Prompts(ctx)
		var timeoutErr *TimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("error = %v (%T), want a timeout", err, err)
		}
	}
	answered := func(t *testing.T, c *Client) {
		t.Helper()
		if _, err := c.Tools(context.Background()); err != nil {
			t.Fatalf("a request the server answers: %v", err)
		}
	}
	type step = func(t *testing.T, c *Client)

	tests := []struct {
		name  string
		steps []step
		// wantStarts is how many server processes were started in all, and
		// wantCancelled how many requests the servers were told are cancelled.
		wantStarts    int
		wantCancelled int
	}{
		{name: "one timeout keeps the server", steps: []step{timeOut, answered}, wantStarts: 1, wantCancelled: 1},
		{name: "a second silent timeout replaces it", steps: []step{timeOut, timeOut, answered}, wantStarts: 2, wantCancelled: 1},
		{name: "a server that answers in between is kept", steps: []step{timeOut, answered, timeOut, answered, timeOut, answered}, wantStarts: 1, wantCancelled: 3},
		{name: "four silent timeouts replace it twice", steps: []step{timeOut, timeOut, timeOut, timeOut, answered}, wantStarts: 3, wantCancelled: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "server.log")
			t.Setenv(cancellableServerEnv, "1")
			t.Setenv(cancellableLogEnv, log)
			transport := NewStdioTransport(os.Args[0])
			transport.SetTimeout(30 * time.Second)
			c := New(transport, testClientInfo())
			defer c.Disconnect()

			for _, step := range tt.steps {
				step(t, c)
			}

			var starts int
			var frames []loggedFrame
			if !pollFor(10*time.Second, func() bool {
				starts, frames = readServerLog(t, log)
				return starts >= tt.wantStarts && len(framesOf(frames, "notifications/cancelled")) >= tt.wantCancelled
			}) || starts != tt.wantStarts {
				t.Fatalf("the server was started %d time(s), want %d", starts, tt.wantStarts)
			}
			if got := len(framesOf(frames, "notifications/cancelled")); got != tt.wantCancelled {
				t.Fatalf("%d request(s) were cancelled, want %d", got, tt.wantCancelled)
			}
		})
	}
}

// TestACallIntoAServerThatStoppedReadingEndsWithTheTimeout is the bound on a
// write through the call a caller makes. The exchange runs under a clock whose
// deadline is the maximum, ten timeouts away by default, and the exchange gate
// is held for as long as the write blocks: a write held to that deadline would
// keep the caller, and everyone queued behind it, ten times longer than the
// timeout they were promised.
func TestACallIntoAServerThatStoppedReadingEndsWithTheTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	// The server answers the handshake and then never reads its input again.
	script := `IFS= read -r line; echo '{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete",` +
		`"supportedVersions":["` + LatestProtocolVersion + `"],"capabilities":{},"ttlMs":600000}}'; exec sleep 30`
	tests := []struct {
		name string
		// maximum is the maximum set on the client, or zero for the default of
		// ten timeouts.
		maximum time.Duration
	}{
		{name: "under the default maximum"},
		{name: "under a maximum of a minute", maximum: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := New(NewStdioTransport("/bin/sh", "-c", script), testClientInfo())
			defer c.Disconnect()
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			c.WithTimeout(timeout)
			if tt.maximum > 0 {
				c.WithMaxTimeout(tt.maximum)
			}

			// Far more than a pipe holds, so the write blocks.
			arguments := map[string]any{"blob": strings.Repeat("x", 4<<20)}
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				_, err := c.CallTool(context.Background(), "upload", arguments)
				done <- err
			}()

			// The write gives up after the timeout, and the server that would
			// not take the frame is then stopped. At worst that takes every
			// stage of the shutdown: the grace to exit by itself, the grace
			// after it is asked to, and the grace for its output. The ceiling
			// is that worst case and a second of slack, which still ends
			// before a write held to the smallest maximum here (ten timeouts)
			// could have given up and been stopped.
			const ceiling = timeout + 2*shutdownGrace + pipeGrace + time.Second
			select {
			case err := <-done:
				var transportErr *TransportError
				if !errors.As(err, &transportErr) {
					t.Fatalf("error = %v (%T), want a transport failure", err, err)
				}
				if took := time.Since(start); took > ceiling {
					t.Fatalf("the call returned after %v, want within %v", took, ceiling)
				}
			case <-time.After(ceiling):
				t.Fatalf("the call had not returned after %v with a timeout of %v", ceiling, timeout)
			}
		})
	}
}

// TestAStdioSendIsBounded asserts a write into the input of a server that has
// stopped reading does not hold the caller for ever: it ends with the deadline,
// as a failure of the channel, and the subprocess is given up.
func TestAStdioSendIsBounded(t *testing.T) {
	tests := []struct {
		name string
		// ctx returns the context of the send.
		ctx func() (context.Context, context.CancelFunc)
		// timeout is the transport timeout.
		timeout time.Duration
	}{
		{
			name: "by the deadline of its context",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 200*time.Millisecond)
			},
			timeout: 30 * time.Second,
		},
		{
			name:    "by the timeout when its context has no deadline",
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			timeout: 200 * time.Millisecond,
		},
		{
			name: "by the timeout when the deadline of its context is further away",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Hour)
			},
			timeout: 200 * time.Millisecond,
		},
		{
			name: "by its context being cancelled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(200*time.Millisecond, cancel)
				return ctx, cancel
			},
			timeout: 30 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The server never reads its input, so the pipe fills.
			tr := NewStdioTransport("/bin/sh", "-c", "exec sleep 30")
			tr.SetTimeout(tt.timeout)
			if err := tr.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer tr.Disconnect()

			ctx, cancel := tt.ctx()
			defer cancel()
			sent := make(chan error, 1)
			go func() { sent <- tr.Send(ctx, strings.Repeat("x", 4<<20)) }()

			select {
			case err := <-sent:
				var transportErr *TransportError
				if !errors.As(err, &transportErr) {
					t.Fatalf("send = %v (%T), want a transport failure", err, err)
				}
				assertStdioTornDown(t, tr)
			case <-time.After(10 * time.Second):
				t.Fatal("a send into a pipe nobody reads had not returned after 10s")
			}
		})
	}
}
