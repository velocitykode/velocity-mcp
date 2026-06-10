package server

import (
	"context"
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
