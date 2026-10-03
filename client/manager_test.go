package client

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/schema"
)

func TestManagerRegisterAndBuild(t *testing.T) {
	m := NewManager()
	built := 0
	m.Register("primary", func() *Client {
		built++
		return New(newFakeTransport(), schema.NewImplementation("c", "1"))
	})

	c1, err := m.Client("primary")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if c1.name != "primary" {
		t.Fatalf("name = %q", c1.name)
	}
	// Memoized: the second lookup returns the same instance.
	c2, _ := m.Client("primary")
	if c1 != c2 || built != 1 {
		t.Fatalf("expected memoized client, built=%d", built)
	}
	// Build always makes a fresh instance.
	if c3, _ := m.Build("primary"); c3 == c1 {
		t.Fatal("Build should not return the memoized client")
	}
}

func TestManagerUnknownClient(t *testing.T) {
	m := NewManager()
	if _, err := m.Client("missing"); err == nil {
		t.Fatal("expected error for unregistered client")
	}
}

func TestManagerReregisterAndDisconnectAll(t *testing.T) {
	m := NewManager()
	m.Register("a", func() *Client { return New(newFakeTransport(), schema.Implementation{}) })
	if _, err := m.Client("a"); err != nil {
		t.Fatalf("client: %v", err)
	}
	// Re-registering replaces the factory and drops the cached client.
	m.Register("a", func() *Client { return New(newFakeTransport(), schema.Implementation{}) })

	c, err := m.Client("a")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	m.DisconnectAll()
	// A fresh build is available after DisconnectAll.
	if _, err := m.Client("a"); err != nil {
		t.Fatalf("client after disconnect-all: %v", err)
	}
}

func TestClientLifecycleHelpers(t *testing.T) {
	f := newFakeTransport()
	c := New(f, schema.Implementation{}).
		WithClientInfo(schema.NewImplementation("named", "9.9")).
		WithTimeout(0)
	if c.ClientInfo().Name != "named" {
		t.Fatalf("client info = %+v", c.ClientInfo())
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	c.Disconnect()
	if c.Connected() {
		t.Fatal("expected disconnected")
	}
}

// hangingDisconnect is a transport whose disconnect never returns until it is
// released, as one outside this package might.
type hangingDisconnect struct {
	*fakeTransport
	release chan struct{}
}

func (h *hangingDisconnect) Disconnect() error {
	<-h.release
	return nil
}

// TestDisconnectAllStopsItsClientsSideBySide asserts what disconnecting a whole
// registry costs: the longest of its clients, not their sum, and never more
// than the bound, whatever one of them does.
func TestDisconnectAllStopsItsClientsSideBySide(t *testing.T) {
	t.Run("stdio servers that have to be asked to stop", func(t *testing.T) {
		// Each server ignores the end of its input, so stopping it waits the
		// two seconds a server is given before it is signalled.
		const servers = 4
		m := NewManager()
		for index := range servers {
			name := "server-" + strconv.Itoa(index)
			m.Register(name, func() *Client {
				return New(NewStdioTransport("/bin/sh", "-c", "exec sleep 30"), schema.Implementation{})
			})
			c, err := m.Client(name)
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			if err := c.transport.Connect(context.Background()); err != nil {
				t.Fatalf("start: %v", err)
			}
		}

		start := time.Now()
		m.DisconnectAll()
		took := time.Since(start)
		t.Logf("disconnecting %d servers took %v", servers, took)
		if took > 5*time.Second {
			t.Fatalf("disconnecting %d servers took %v, want about the time one takes", servers, took)
		}
	})

	t.Run("a disconnect that never returns does not hold the caller", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		m := NewManager()
		m.Register("stuck", func() *Client {
			return New(&hangingDisconnect{fakeTransport: newFakeTransport(), release: release}, schema.Implementation{})
		})
		if _, err := m.Client("stuck"); err != nil {
			t.Fatalf("client: %v", err)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			m.DisconnectAll()
		}()
		select {
		case <-done:
		case <-time.After(disconnectAllCeiling):
			t.Fatalf("DisconnectAll had not returned after %v", disconnectAllCeiling)
		}
		// The registry is cleared either way: the next lookup builds anew.
		if _, err := m.Client("stuck"); err != nil {
			t.Fatalf("client after disconnect-all: %v", err)
		}
	})
}

// disconnectAllCeiling is how long the test gives DisconnectAll to give up on a
// client that never disconnects: its bound and a margin.
const disconnectAllCeiling = 9 * time.Second
