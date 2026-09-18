package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/schema"
)

// This file covers the gate that serializes exchanges. It is held for a whole
// network round trip, so a caller waiting behind one must be able to withdraw:
// the exchange ahead of it may be waiting on a server that never answers.

// waitForever is how long a test waits for a call that should have returned at
// once. It is a deadline for reporting a hang, never a synchronisation device.
const waitForever = 10 * time.Second

// blockingTransport answers nothing until it is released. It reports when a
// receive starts, so a test knows an exchange is under way without timing
// anything.
type blockingTransport struct {
	entered chan struct{}
	release chan struct{}

	mu   sync.Mutex
	sent []string
	once sync.Once
}

var _ Transport = (*blockingTransport)(nil)

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingTransport) Connect(context.Context) error { return nil }
func (b *blockingTransport) Disconnect() error             { return nil }
func (b *blockingTransport) SetTimeout(time.Duration)      {}
func (b *blockingTransport) Recipe() Recipe                { return Recipe{Driver: "blocking"} }

func (b *blockingTransport) Send(_ context.Context, message string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, message)
	return nil
}

func (b *blockingTransport) Receive(ctx context.Context) (string, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return "", newError("the blocking transport was released")
	case <-ctx.Done():
		return "", NewTransportError("the blocking transport was abandoned", ctx.Err())
	}
}

// frames returns what the client has put on the wire so far.
func (b *blockingTransport) frames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

// TestAWaitingRequestHonoursItsContext asserts a second request does not have to
// outlive the first: while one exchange holds the connection, a caller whose own
// context is withdrawn returns with that context's error and sends nothing.
func TestAWaitingRequestHonoursItsContext(t *testing.T) {
	transport := newBlockingTransport()
	c := New(transport, schema.NewImplementation("Acme MCP App", "9.9.9"))

	// The first caller occupies the gate for as long as the test needs: its
	// handshake is sent and the answer never comes.
	first := make(chan struct{})
	go func() {
		defer close(first)
		_ = c.Connect(context.Background())
	}()
	<-transport.entered

	sentBefore := len(transport.frames())

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- c.Ping(ctx) }()
	cancel()

	select {
	case err := <-second:
		if err == nil {
			t.Fatal("the waiting request reported success")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the caller's own cancellation", err)
		}
	case <-time.After(waitForever):
		t.Fatal("a cancelled request waited for an exchange it had given up on")
	}

	if got := len(transport.frames()); got != sentBefore {
		t.Fatalf("the abandoned request put %d extra frame(s) on the wire", got-sentBefore)
	}

	close(transport.release)
	<-first
}

// TestAnExpiredDeadlineNeverReachesTheWire asserts the gate is not the only
// check: a request whose deadline has already passed sends nothing even when
// nothing is holding the connection, which a select over a free gate and a done
// context would decide at random.
func TestAnExpiredDeadlineNeverReachesTheWire(t *testing.T) {
	transport := newBlockingTransport()
	c := New(transport, schema.NewImplementation("Acme MCP App", "9.9.9"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for range 50 {
		err := c.Ping(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the caller's own cancellation", err)
		}
	}
	if got := transport.frames(); len(got) != 0 {
		t.Fatalf("an abandoned request reached the wire: %v", got)
	}
}

// TestExchangesStaySerialised asserts the gate still does its job: concurrent
// callers take it in turn, so frames of two exchanges never interleave on a
// transport that carries one reply at a time.
func TestExchangesStaySerialised(t *testing.T) {
	f := newFakeTransport()
	c := New(f, schema.NewImplementation("Acme MCP App", "9.9.9"))
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Ping(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("ping: %v", err)
	}
}
