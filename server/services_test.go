package server_test

import (
	"context"
	"testing"

	"github.com/velocitykode/velocity/contract"

	"github.com/velocitykode/velocity/app"

	"github.com/velocitykode/velocity-mcp/server"
)

// Compile-time assertion that *server.Server satisfies the interface velocity
// uses to auto-wire the event dispatcher when registered in Extensions.
var _ contract.EventDispatcherAware = (*server.Server)(nil)

func TestRegisterAndFromServices(t *testing.T) {
	s := &app.Services{}
	srv := server.New("demo", "1.0.0")

	if err := server.RegisterServices(s, srv); err != nil {
		t.Fatalf("register: %v", err)
	}

	got := server.FromServices(s)
	if got != srv {
		t.Fatalf("FromServices returned %v, want the registered server", got)
	}

	// Registering twice fails (the extension key is already taken).
	if err := server.RegisterServices(s, server.New("other", "2")); err == nil {
		t.Fatal("expected an error registering a second mcp server")
	}
}

func TestFromServicesNil(t *testing.T) {
	if server.FromServices(nil) != nil {
		t.Fatal("FromServices(nil) should be nil")
	}
	if server.FromServices(&app.Services{}) != nil {
		t.Fatal("FromServices with no registration should be nil")
	}
}

// TestServerEventDispatcherWiring verifies the dispatcher can be wired and
// invoked through the auto-wire interface.
func TestServerEventDispatcherWiring(t *testing.T) {
	var aware contract.EventDispatcherAware = server.New("demo", "1.0.0")
	called := false
	aware.SetEventDispatcher(func(context.Context, any) error {
		called = true
		return nil
	})
	// Drive an initialize through to confirm the wired dispatcher fires.
	srv := aware.(*server.Server)
	srv.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`), "")
	if !called {
		t.Fatal("wired dispatcher was not invoked")
	}
}
