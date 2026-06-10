package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// syncBuffer is a concurrency-safe writer for capturing stdio output while the
// loop runs in a goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNewStdio_DefaultsToOSStreams(t *testing.T) {
	st := NewStdio(&stubServer{}, nil, nil)
	if st.in == nil || st.out == nil {
		t.Fatal("nil reader/writer should fall back to os.Stdin/os.Stdout")
	}
}

func TestStdio_Run_RequestResponse(t *testing.T) {
	srv := newTestServer(t)
	in := strings.NewReader(string(initializeRequest(1)) + "\n" + string(callToolRequest(2, 2, 3)) + "\n")
	out := &syncBuffer{}

	st := NewStdio(srv, in, out)
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := nonEmptyLines(out.String())
	if len(lines) != 2 {
		t.Fatalf("want 2 response lines, got %d: %q", len(lines), out.String())
	}

	initResp := decodeResponse(t, []byte(lines[0]))
	if initResp.Error != nil {
		t.Fatalf("initialize errored: %+v", initResp.Error)
	}
	callResp := decodeResponse(t, []byte(lines[1]))
	if callResp.Error != nil {
		t.Fatalf("tools/call errored: %+v", callResp.Error)
	}
	if !bytes.Contains(callResp.Result, []byte("5")) {
		t.Fatalf("tools/call result missing sum: %s", callResp.Result)
	}

	// The session id from initialize must be propagated to the second call.
	if st.SessionID() != "fixed-session" {
		t.Fatalf("session id not retained: %q", st.SessionID())
	}
}

func TestStdio_Run_SessionPropagation(t *testing.T) {
	var seen []string
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		seen = append(seen, sessionID)
		// First call assigns a session; subsequent calls should carry it.
		if bytes.Contains(raw, []byte(`"first"`)) {
			return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true, SessionID: "S1"}
		}
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(2), nil), HasResponse: true}
	}}
	in := strings.NewReader(`{"k":"first"}` + "\n" + `{"k":"second"}` + "\n")
	out := &syncBuffer{}
	st := NewStdio(stub, in, out)
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 2 || seen[0] != "" || seen[1] != "S1" {
		t.Fatalf("session propagation wrong: %v", seen)
	}
}

func TestStdio_Run_Notification_NoReply(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{HasResponse: false}
	}}
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	out := &syncBuffer{}
	st := NewStdio(stub, in, out)
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("notification should produce no output, got %q", out.String())
	}
}

func TestStdio_Run_BlankLinesIgnored(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true}
	}}
	in := strings.NewReader("\n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n")
	out := &syncBuffer{}
	st := NewStdio(stub, in, out)
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stub.callCount() != 1 {
		t.Fatalf("blank lines should be skipped; want 1 call, got %d", stub.callCount())
	}
	if len(nonEmptyLines(out.String())) != 1 {
		t.Fatalf("want 1 reply, got %q", out.String())
	}
}

func TestStdio_Run_EOFEndsCleanly(t *testing.T) {
	st := NewStdio(&stubServer{fn: passthroughOK(t)}, strings.NewReader(""), &syncBuffer{})
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("Run on empty input should return nil, got %v", err)
	}
}

func TestStdio_Run_ContextCancellation(t *testing.T) {
	// A reader that blocks forever simulates a live stdin with no data; the
	// loop must still stop when the context is cancelled.
	pr, pw := io.Pipe()
	defer pw.Close()

	stub := &stubServer{fn: passthroughOK(t)}
	st := NewStdio(stub, pr, &syncBuffer{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled Run should return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestStdio_Run_CancelWhileHandlerPending(t *testing.T) {
	// Inject one message then keep the stream open; cancel mid-stream and
	// ensure the loop exits even though more reads could occur.
	pr, pw := io.Pipe()
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true}
	}}
	out := &syncBuffer{}
	st := NewStdio(stub, pr, out)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx) }()

	if _, err := pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Give the loop a moment to process the line, then cancel.
	waitFor(t, func() bool { return len(nonEmptyLines(out.String())) == 1 })
	cancel()
	pw.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestStdio_Run_NilContext(t *testing.T) {
	st := NewStdio(&stubServer{fn: passthroughOK(t)}, strings.NewReader(""), &syncBuffer{})
	var nilCtx context.Context // a nil-valued context, exercising the tolerance branch
	if err := st.Run(nilCtx); err != nil {
		t.Fatalf("Run(nil): %v", err)
	}
}

// errReader returns a non-EOF read error to exercise the read-error branch.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestStdio_Run_ReadErrorEndsCleanly(t *testing.T) {
	st := NewStdio(&stubServer{fn: passthroughOK(t)}, errReader{err: errors.New("boom")}, &syncBuffer{})
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("read error should end the session without surfacing, got %v", err)
	}
}

// failWriter fails every write to exercise Send's error path.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestStdio_Run_WriteErrorSurfaces(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true}
	}}
	st := NewStdio(stub, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), failWriter{})
	if err := st.Run(context.Background()); err == nil {
		t.Fatal("expected write error to be surfaced from Run")
	}
}

// unmarshalableResponse returns a response whose error data cannot be JSON
// marshalled (a func value), exercising the encode-error branch.
func unmarshalableResponse() *jsonrpc.Response {
	return jsonrpc.NewErrorResponse(jsonrpc.IntID(1), jsonrpc.NewError(jsonrpc.CodeInternalError, "x").WithData(func() {}))
}

func TestStdio_Run_MarshalErrorDropsFrame(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{Response: unmarshalableResponse(), HasResponse: true}
	}}
	out := &syncBuffer{}
	st := NewStdio(stub, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), out)
	if err := st.Run(context.Background()); err != nil {
		t.Fatalf("unmarshalable response should be dropped, not surfaced: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("unmarshalable response should produce no output, got %q", out.String())
	}
}

func TestStdio_Send_WritesLineDelimited(t *testing.T) {
	out := &syncBuffer{}
	st := NewStdio(&stubServer{}, strings.NewReader(""), out)
	if err := st.Send(context.Background(), []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := out.String(); got != `{"a":1}`+"\n" {
		t.Fatalf("Send framing wrong: %q", got)
	}
}

func TestStdio_Send_Concurrent(t *testing.T) {
	out := &syncBuffer{}
	st := NewStdio(&stubServer{}, strings.NewReader(""), out)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = st.Send(context.Background(), []byte(`{"x":1}`))
		}()
	}
	wg.Wait()
	if got := len(nonEmptyLines(out.String())); got != 50 {
		t.Fatalf("want 50 framed messages, got %d", got)
	}
}

func TestServeStdio_DrivesServer(t *testing.T) {
	// ServeStdio wires os.Stdin/os.Stdout; with an empty stdin substitute it
	// would block, so we exercise the wrapper via NewStdio paths above and only
	// assert ServeStdio returns promptly when its context is already done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Use a stub server; ServeStdio reads os.Stdin which is typically empty in
	// test runs (EOF) so it returns. The cancelled context guards against hangs.
	done := make(chan error, 1)
	go func() { done <- ServeStdio(ctx, newTestServer(t)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeStdio: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeStdio did not return")
	}
}

// passthroughOK returns a handler that always replies with an empty success.
func passthroughOK(t *testing.T) func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
	return func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true}
	}
}

// nonEmptyLines splits s on newlines and drops empty entries.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// waitFor polls cond until it is true or a short timeout elapses.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
