package server

import (
	"context"
	"errors"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

func TestNewDefaults(t *testing.T) {
	s := New("demo", "2.0.0")
	if s.Name() != "demo" || s.Version() != "2.0.0" {
		t.Fatalf("name/version = %q/%q", s.Name(), s.Version())
	}
	if s.Instructions() != defaultInstructions {
		t.Fatalf("instructions = %q", s.Instructions())
	}
	c := s.createContext(context.Background(), "")
	for _, cap := range []string{CapabilityTools, CapabilityResources, CapabilityPrompts} {
		if !c.HasCapability(cap) {
			t.Fatalf("default capability %q missing", cap)
		}
	}
	if len(c.SupportedProtocolVersions()) != 4 {
		t.Fatalf("default versions = %v", c.SupportedProtocolVersions())
	}
	if c.SupportedProtocolVersions()[0] != LatestProtocolVersion {
		t.Fatal("first supported version should be the latest")
	}
}

func TestSetEventDispatcher(t *testing.T) {
	s := New("demo", "1.0.0")

	var got any
	s.SetEventDispatcher(func(ctx context.Context, event any) error {
		got = event
		return nil
	})
	s.dispatch(context.Background(), "hello")
	if got != "hello" {
		t.Fatalf("dispatched event = %v", got)
	}

	// Detaching the dispatcher makes dispatch a no-op.
	s.SetEventDispatcher(nil)
	got = nil
	s.dispatch(context.Background(), "ignored")
	if got != nil {
		t.Fatal("detached dispatcher should be a no-op")
	}
}

func TestDispatchSwallowsError(t *testing.T) {
	s := New("demo", "1.0.0")
	s.SetEventDispatcher(func(ctx context.Context, event any) error {
		return errors.New("boom")
	})
	// Should not panic and should not propagate the error.
	s.dispatch(context.Background(), "x")
}

func TestSessionIDGenerator(t *testing.T) {
	s := New("demo", "1.0.0")
	s.SetSessionIDGenerator(func() string { return "fixed-id" })
	if s.sessionID() != "fixed-id" {
		t.Fatalf("session id = %q", s.sessionID())
	}
	// nil restores the default (non-empty random id).
	s.SetSessionIDGenerator(nil)
	if s.sessionID() == "" {
		t.Fatal("default session id should be non-empty")
	}
}

func TestRandomSessionID(t *testing.T) {
	a, b := randomSessionID(), randomSessionID()
	if a == "" || b == "" {
		t.Fatal("random session id should be non-empty")
	}
	if a == b {
		t.Fatal("random session ids should differ")
	}
	if len(a) != 32 {
		t.Fatalf("session id length = %d want 32", len(a))
	}
}

// TestHandleFallbackInitialize verifies the server package alone (without the
// methods package imported) can complete an initialize handshake via the
// built-in fallback handler.
func TestHandleFallbackInitialize(t *testing.T) {
	s := New("demo", "1.0.0")
	s.SetSessionIDGenerator(func() string { return "sess-1" })

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	res := s.Handle(context.Background(), raw, "")
	if !res.HasResponse || res.Response == nil {
		t.Fatal("expected a response")
	}
	if res.Response.Error != nil {
		t.Fatalf("unexpected error: %+v", res.Response.Error)
	}
	if res.SessionID != "sess-1" {
		t.Fatalf("session id = %q", res.SessionID)
	}
}

func TestHandleUnsupportedVersion(t *testing.T) {
	s := New("demo", "1.0.0")
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	res := s.Handle(context.Background(), raw, "")
	if res.Response.Error == nil {
		t.Fatal("expected an error for unsupported version")
	}
	if res.Response.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("error code = %d", res.Response.Error.Code)
	}
	// No session is assigned on a failed initialize.
	if res.SessionID != "" {
		t.Fatalf("session id should be empty on failure, got %q", res.SessionID)
	}
}

func TestHandleParseError(t *testing.T) {
	s := New("demo", "1.0.0")
	res := s.Handle(context.Background(), []byte("{not json"), "")
	if res.Response.Error == nil || res.Response.Error.Code != jsonrpc.CodeParseError {
		t.Fatalf("expected parse error, got %+v", res.Response.Error)
	}
}

func TestHandleNotification(t *testing.T) {
	s := New("demo", "1.0.0")
	// A well-formed notification (no id) produces no response.
	raw := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	res := s.Handle(context.Background(), raw, "")
	if res.HasResponse {
		t.Fatal("a notification should produce no response")
	}
}

func TestHandleMalformedNotification(t *testing.T) {
	s := New("demo", "1.0.0")
	// No id (notification) but a bad version -> invalid request error reply.
	raw := []byte(`{"jsonrpc":"1.0","method":"x"}`)
	res := s.Handle(context.Background(), raw, "")
	if !res.HasResponse || res.Response.Error == nil {
		t.Fatal("malformed notification should yield an error response")
	}
}

func TestHandleNilContext(t *testing.T) {
	s := New("demo", "1.0.0")
	// A nil context must not panic; Handle falls back to context.Background.
	var nilCtx context.Context
	res := s.Handle(nilCtx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
	if !res.HasResponse {
		t.Fatal("expected a response")
	}
}

func TestWithErrorGenericFailure(t *testing.T) {
	// A handler error that is not a *jsonrpc.Error becomes a generic internal
	// error response, never leaking the underlying message.
	r := HandleResult{}.withError(jsonrpc.IntID(1), errors.New("secret internal detail"))
	if r.Response.Error == nil || r.Response.Error.Code != jsonrpc.CodeInternalError {
		t.Fatalf("expected internal error, got %+v", r.Response.Error)
	}
	if r.Response.Error.Message == "secret internal detail" {
		t.Fatal("internal error detail leaked to client")
	}
}

func TestWithErrorRPCError(t *testing.T) {
	rpcErr := jsonrpc.NewError(jsonrpc.CodeInvalidParams, "bad params")
	r := HandleResult{}.withError(jsonrpc.IntID(1), rpcErr)
	if r.Response.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected the rpc error preserved, got %+v", r.Response.Error)
	}
}
