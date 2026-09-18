package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

func TestTransportErrorIsAlsoAClientError(t *testing.T) {
	cause := io.ErrUnexpectedEOF
	err := error(NewTransportError("the endpoint rejected the request", cause))

	if err.Error() != "the endpoint rejected the request: unexpected EOF" {
		t.Fatalf("error = %q", err.Error())
	}

	var transportErr *TransportError
	if !errors.As(err, &transportErr) {
		t.Fatal("a transport failure must match errors.As for *TransportError")
	}
	var clientErr *Error
	if !errors.As(err, &clientErr) {
		t.Fatal("a transport failure must also be a client error")
	}
	if clientErr.Message != "the endpoint rejected the request" {
		t.Fatalf("client error message = %q", clientErr.Message)
	}
	if !errors.Is(err, cause) {
		t.Fatal("a transport failure must unwrap to its cause")
	}
	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatal("a transport failure is not a timeout")
	}
}

func TestTimeoutErrorIsAlsoATransportError(t *testing.T) {
	cause := context.DeadlineExceeded
	err := error(NewTimeoutError("timed out while waiting for server response", cause))

	if err.Error() != "timed out while waiting for server response: context deadline exceeded" {
		t.Fatalf("error = %q", err.Error())
	}

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatal("a timeout must match errors.As for *TimeoutError")
	}
	var transportErr *TransportError
	if !errors.As(err, &transportErr) {
		t.Fatal("a timeout must also be a transport failure")
	}
	var clientErr *Error
	if !errors.As(err, &clientErr) {
		t.Fatal("a timeout must also be a client error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a timeout must unwrap to its cause")
	}
}

func TestTypedErrorsWithoutACause(t *testing.T) {
	transportErr := NewTransportError("no channel", nil)
	if transportErr.Error() != "no channel" {
		t.Fatalf("error = %q", transportErr.Error())
	}
	timeoutErr := NewTimeoutError("no answer", nil)
	if timeoutErr.Error() != "no answer" {
		t.Fatalf("error = %q", timeoutErr.Error())
	}

	// A nil error of either type must describe itself rather than panic.
	var nilTransport *TransportError
	if nilTransport.Error() != "<nil client transport error>" || nilTransport.Unwrap() != nil {
		t.Fatalf("nil transport error = %q", nilTransport.Error())
	}
	var nilTimeout *TimeoutError
	if nilTimeout.Error() != "<nil client timeout error>" || nilTimeout.Unwrap() != nil {
		t.Fatalf("nil timeout error = %q", nilTimeout.Error())
	}
}

// assertStdioTornDown asserts the subprocess was stopped: both halves of the
// channel report a dead transport by name, rather than waiting again on a
// stream that is out of step with the server.
func assertStdioTornDown(t *testing.T, tr *StdioTransport) {
	t.Helper()
	const want = "transport is not connected"
	if _, err := tr.Receive(context.Background()); err == nil || err.Error() != want {
		t.Fatalf("receive after the teardown = %v, want %q", err, want)
	}
	if err := tr.Send(context.Background(), `{"jsonrpc":"2.0","id":9,"method":"ping"}`); err == nil || err.Error() != want {
		t.Fatalf("send after the teardown = %v, want %q", err, want)
	}
}

func TestStdioReceiveTimeoutIsATypedTimeout(t *testing.T) {
	// `sleep` never writes, so the receive can only end in a timeout.
	tr := NewStdioTransport("sleep", "5")
	tr.SetTimeout(50 * time.Millisecond)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	_, err := tr.Receive(context.Background())

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if err.Error() != "timed out while waiting for server response" {
		t.Fatalf("error = %q", err.Error())
	}
	assertStdioTornDown(t, tr)
}

func TestStdioReceiveHonoursAContextDeadline(t *testing.T) {
	tr := NewStdioTransport("sleep", "5")
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := tr.Receive(ctx)

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to carry the context deadline", err)
	}
	// A deadline that elapsed is a timeout like any other, so the subprocess is
	// stopped with it.
	assertStdioTornDown(t, tr)
}

// TestStdioReceiveSeparatesCancellationFromATimeout pins that a caller
// withdrawing the request is not reported as a timeout. The distinction is
// load bearing: a timeout is a transport failure, which the connection probe
// retries with the older handshake, while a cancelled context is the caller
// giving up and no handshake would do better.
func TestStdioReceiveSeparatesCancellationFromATimeout(t *testing.T) {
	tr := NewStdioTransport("sleep", "5")
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tr.Receive(ctx)

	if err == nil {
		t.Fatal("expected the receive to fail")
	}
	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want a cancellation rather than a timeout", err)
	}
	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		t.Fatalf("error = %v, want a cancellation rather than a transport failure", err)
	}
	var clientErr *Error
	if !errors.As(err, &clientErr) {
		t.Fatalf("error = %v, want a client error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to carry the cancellation", err)
	}
	if err.Error() != "the wait for a response from subprocess [sleep] was cancelled: context canceled" {
		t.Fatalf("error = %q", err.Error())
	}
	// The exchange was abandoned mid-flight, so the subprocess goes with it: the
	// stream it owns can no longer be trusted to be in step.
	assertStdioTornDown(t, tr)
}

func TestStdioTransportCarriesNoProtocolHooks(t *testing.T) {
	// A transport without a header channel implements neither hook, and the
	// client keeps working over it: the negotiation still runs, the frames just
	// carry no headers.
	var transport Transport = NewStdioTransport("cat")
	if _, ok := transport.(HeaderSender); ok {
		t.Fatal("the stdio transport must not claim it can carry headers")
	}
	if _, ok := transport.(ProtocolAware); ok {
		t.Fatal("the stdio transport must not claim it tracks the protocol version")
	}
}

// plainTransport is a Transport with neither optional protocol hook, standing
// in for a transport written outside this package.
type plainTransport struct {
	inner *fakeTransport
}

var _ Transport = (*plainTransport)(nil)

func (p *plainTransport) Connect(ctx context.Context) error { return p.inner.Connect(ctx) }
func (p *plainTransport) Disconnect() error                 { return p.inner.Disconnect() }
func (p *plainTransport) SetTimeout(d time.Duration)        { p.inner.SetTimeout(d) }
func (p *plainTransport) Recipe() Recipe                    { return p.inner.Recipe() }
func (p *plainTransport) Send(ctx context.Context, message string) error {
	return p.inner.Send(ctx, message)
}

func (p *plainTransport) Receive(ctx context.Context) (string, error) { return p.inner.Receive(ctx) }

func TestClientWorksOverATransportWithoutTheProtocolHooks(t *testing.T) {
	inner := newFakeTransport()
	inner.on("server/discover", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"supportedVersions": []any{LatestProtocolVersion},
			"capabilities":      map[string]any{},
		})
		return resp
	})
	c := New(&plainTransport{inner: inner}, testClientInfo())

	version, err := c.ProtocolVersion(context.Background())
	if err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if version != LatestProtocolVersion {
		t.Fatalf("negotiated version = %q, want %q", version, LatestProtocolVersion)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// The metadata still travels in the request body, which is the only channel
	// such a transport has.
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.versions) != 0 {
		t.Fatalf("the version was announced to a transport that cannot take it: %v", inner.versions)
	}
	var frame sentFrame
	if err := json.Unmarshal([]byte(inner.sent[len(inner.sent)-1]), &frame); err != nil {
		t.Fatalf("last frame: %v", err)
	}
	if meta := frame.meta(); meta == nil || meta[MetaProtocolVersion] != LatestProtocolVersion {
		t.Fatalf("last frame metadata = %v", frame.Params["_meta"])
	}
}
