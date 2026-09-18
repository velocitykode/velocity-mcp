package server

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
)

func newTestContext() *Context {
	return NewContext(
		schema.NewImplementation("demo", "1.0.0"),
		"instructions",
		supportedProtocolVersions(),
		defaultCapabilities(),
	)
}

func TestContextPerPage(t *testing.T) {
	c := newTestContext()
	intp := func(n int) *int { return &n }
	tests := []struct {
		name      string
		requested *int
		want      int
	}{
		{"absent falls back to default", nil, defaultPageSize},
		{"honors positive requested", intp(10), 10},
		{"caps at max", intp(1000), defaultMaxPageSize},
		// An explicit per_page of 0 yields an empty page (min(0, max) = 0):
		// the default only fires on an absent (nil) value.
		{"explicit zero stays zero", intp(0), 0},
		// A negative explicit value is likewise passed through (not defaulted).
		{"explicit negative stays negative", intp(-5), -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.PerPage(tt.requested); got != tt.want {
				t.Fatalf("PerPage(%v) = %d want %d", tt.requested, got, tt.want)
			}
		})
	}
}

func TestContextRequestContext(t *testing.T) {
	// A freshly constructed context defaults to a non-nil background context.
	c := newTestContext()
	if c.RequestContext() == nil {
		t.Fatal("RequestContext should never be nil")
	}

	// withRequestContext records a supplied context and a nil falls back to
	// background.
	type ctxKey string
	const key ctxKey = "k"
	parent := context.WithValue(context.Background(), key, "v")
	c.withRequestContext(parent)
	if c.RequestContext().Value(key) != "v" {
		t.Fatal("RequestContext did not carry the supplied context")
	}
	var nilCtx context.Context // deliberately nil to exercise the fallback
	c.withRequestContext(nilCtx)
	if c.RequestContext() == nil {
		t.Fatal("nil request context should fall back to background")
	}
}

func TestContextNegotiatedVersion(t *testing.T) {
	c := newTestContext()
	if c.NegotiatedVersion() != "" {
		t.Fatal("negotiated version should start empty")
	}
	c.SetNegotiatedVersion("2025-06-18")
	if c.NegotiatedVersion() != "2025-06-18" {
		t.Fatalf("negotiated = %q", c.NegotiatedVersion())
	}
}

func TestContextStateConcurrent(t *testing.T) {
	c := newTestContext()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c.SetState("k", n)
			_, _ = c.State("k")
			_ = c.NegotiatedVersion()
			c.SetNegotiatedVersion("v")
		}(i)
	}
	wg.Wait()
	if _, ok := c.State("k"); !ok {
		t.Fatal("state key should be present")
	}
	if _, ok := c.State("missing"); ok {
		t.Fatal("missing key should not be present")
	}
}

func TestContextCapabilitiesCopy(t *testing.T) {
	c := newTestContext()
	caps := c.Capabilities()
	caps["injected"] = true
	if c.HasCapability("injected") {
		t.Fatal("Capabilities should return a copy")
	}
}

// TestNotifyWritesTheEncodedFrame asserts the frame a handler pushes is the
// JSON-RPC 2.0 notification object the specification describes: the version,
// the method, the params it was given, and no id, since a notification is the
// message a peer never answers. The bytes are spelled out here rather than
// rebuilt from the encoder, so a change in what goes on the wire has to be
// stated.
func TestNotifyWritesTheEncodedFrame(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params any
		want   string
	}{
		{
			name: "params object", method: "notifications/subscriptions/acknowledged",
			params: map[string]any{"notifications": map[string]any{}},
			want:   `{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"notifications":{}}}`,
		},
		{
			name: "no params", method: "notifications/cancelled", params: nil,
			want: `{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
		},
		{
			name: "empty params object is still stated", method: "notifications/progress",
			params: map[string]any{},
			want:   `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var frames [][]byte
			c := newTestContext().withEmitter(func(msg []byte) error {
				frames = append(frames, msg)
				return nil
			})

			if err := c.Notify(tt.method, tt.params); err != nil {
				t.Fatalf("Notify: %v", err)
			}
			if len(frames) != 1 {
				t.Fatalf("emitted %d frames, want 1", len(frames))
			}
			if got := string(frames[0]); got != tt.want {
				t.Fatalf("frame =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// TestNotifyRefusesParamsItCannotEncode asserts params no peer could be sent
// are reported to the handler instead of reaching the transport. A partial or
// empty frame on a stream the client is reading for notifications is worse than
// a failed request: the client would take it for a message the server meant to
// send.
func TestNotifyRefusesParamsItCannotEncode(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic

	tests := []struct {
		name   string
		params any
	}{
		{"a channel", map[string]any{"ch": make(chan int)}},
		{"a function", map[string]any{"fn": func() {}}},
		{"a value that is not a number", map[string]any{"n": math.Inf(1)}},
		{"a bag that holds itself", cyclic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emitted := 0
			c := newTestContext().withEmitter(func([]byte) error {
				emitted++
				return nil
			})

			err := c.Notify("notifications/progress", tt.params)
			if err == nil {
				t.Fatal("params that cannot be encoded were accepted")
			}
			if emitted != 0 {
				t.Fatalf("a refused notification still wrote %d frames", emitted)
			}
		})
	}
}

// TestNotifyWithoutAStreamIsANoOp asserts a handler may push frames without
// knowing whether the transport carrying it can deliver them.
func TestNotifyWithoutAStreamIsANoOp(t *testing.T) {
	if err := newTestContext().Notify("notifications/progress", map[string]any{"a": 1}); err != nil {
		t.Fatalf("Notify without a sink: %v", err)
	}
}

// TestNotifySurfacesTheWriteFailure asserts a sink that cannot write is
// reported rather than swallowed, so a handler answers for a frame the client
// never received.
func TestNotifySurfacesTheWriteFailure(t *testing.T) {
	want := errors.New("stream closed")
	c := newTestContext().withEmitter(func([]byte) error { return want })

	if err := c.Notify("notifications/progress", nil); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
