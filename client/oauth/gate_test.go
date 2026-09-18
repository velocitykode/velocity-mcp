package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// answerTimeout is how long a test waits for a flow that must not block before
// declaring it stuck. It is a failure deadline, never a synchronisation device.
const answerTimeout = 10 * time.Second

// blockingAS is a fake authorization server that holds every metadata request
// open until the test hands it back, so a flow can be parked inside discovery
// while other flows start.
type blockingAS struct {
	srv     *httptest.Server
	entered chan struct{} // one value per metadata request that has started
	release chan struct{} // one value consumed per metadata request to finish it
}

func newBlockingAS(t *testing.T) *blockingAS {
	t.Helper()
	a := &blockingAS{entered: make(chan struct{}), release: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	a.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		// Hand back anything still parked, so a failed assertion reports itself
		// instead of hanging the shutdown of the server.
		for range cap(a.release) {
			select {
			case a.release <- struct{}{}:
			default:
			}
		}
		a.srv.Close()
	})

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		a.entered <- struct{}{}
		<-a.release
		writeJSON(w, map[string]any{
			"issuer":                           a.srv.URL,
			"authorization_endpoint":           a.srv.URL + "/authorize",
			"token_endpoint":                   a.srv.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	return a
}

// serveOne lets exactly one held metadata request through.
func (a *blockingAS) serveOne(t *testing.T) {
	t.Helper()
	<-a.entered
	a.release <- struct{}{}
}

// atomicClock is a clock a test can move while flows are in flight.
type atomicClock struct{ nanos atomic.Int64 }

func (k *atomicClock) set(at time.Time)     { k.nanos.Store(at.UnixNano()) }
func (k *atomicClock) add(by time.Duration) { k.nanos.Add(int64(by)) }
func (k *atomicClock) now() time.Time       { return time.Unix(0, k.nanos.Load()) }
func (k *atomicClock) attach(c *Client)     { c.now = k.now }

// startFlow runs one authorization start in the background and reports its
// error on the returned channel.
func startFlow(ctx context.Context, c *Client) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, _, err := c.AuthorizationURL(ctx, "")
		done <- err
	}()
	return done
}

func TestFlowsWaitingOnAnotherFlowObserveTheirContext(t *testing.T) {
	// Discovery and registration are serialised so a burst of first flows does
	// the work once. A flow that arrives while another holds that turn still
	// answers to its own context: a browser that has gone away, or a request
	// whose deadline has passed, must not be left parked behind an authorization
	// server that is slow to reply to somebody else.
	tests := []struct {
		name    string
		context func(t *testing.T) context.Context
		wantErr error
	}{
		{
			name: "a cancelled flow gives up",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErr: context.Canceled,
		},
		{
			name: "a flow past its deadline gives up",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
				t.Cleanup(cancel)
				return ctx
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newBlockingAS(t)
			c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")

			holding := startFlow(context.Background(), c)
			<-as.entered // the first flow is inside discovery, holding the turn.

			waiting := startFlow(tt.context(t), c)
			select {
			case err := <-waiting:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("waiting flow error = %v, want one wrapping %v", err, tt.wantErr)
				}
			case <-time.After(answerTimeout):
				t.Fatal("the waiting flow stayed blocked behind another flow's discovery instead of observing its context")
			}

			// The flow that held the turn still completes normally.
			as.release <- struct{}{}
			if err := <-holding; err != nil {
				t.Fatalf("the holding flow failed: %v", err)
			}
		})
	}
}

func TestAFlowServedFromTheMemoDoesNotWaitOnARefresh(t *testing.T) {
	// A flow whose metadata is already memoized needs nothing from the network,
	// so it must not queue behind the flow that is refreshing that metadata
	// after discoveryTTL.
	as := newBlockingAS(t)
	c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL+"/mcp", "", "")
	clock := &atomicClock{}
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock.set(base)
	clock.attach(c)

	// Warm the memo.
	warming := startFlow(context.Background(), c)
	as.serveOne(t)
	if err := <-warming; err != nil {
		t.Fatalf("warm-up flow: %v", err)
	}

	// Age the memo past its window and start the flow that refreshes it, which
	// parks in the metadata request. Moving the clock back inside the window
	// then puts a live memo in front of the next flow while the refresh is still
	// in flight, which is exactly the overlap a long metadata fetch produces.
	clock.add(discoveryTTL + time.Second)
	refreshing := startFlow(context.Background(), c)
	<-as.entered
	clock.set(base.Add(time.Minute))

	memoized := startFlow(context.Background(), c)
	select {
	case err := <-memoized:
		if err != nil {
			t.Fatalf("a start served from the memo failed: %v", err)
		}
	case <-time.After(answerTimeout):
		t.Fatal("a start whose metadata was already memoized queued behind an in-flight refresh")
	}

	as.release <- struct{}{}
	if err := <-refreshing; err != nil {
		t.Fatalf("refreshing flow: %v", err)
	}
}
