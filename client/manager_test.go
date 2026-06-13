package client

import (
	"context"
	"testing"

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
