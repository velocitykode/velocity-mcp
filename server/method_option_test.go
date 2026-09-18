package server_test

import (
	"testing"

	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// pinnedInitialize replaces the built-in handshake with one that settles on a
// version of its own choosing, as an application speaking a private revision
// would.
type pinnedInitialize struct{ version string }

func (m pinnedInitialize) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	c.SetNegotiatedVersion(m.version)
	return jsonrpc.NewResult(req.ID, map[string]any{"protocolVersion": m.version})
}

// TestWithMethodReplacesInitialize asserts replacing the handshake is a
// supported use of the option rather than an accident: the replacement answers,
// and the machinery the Server owns around it (the session id and the session
// event) still runs, reporting the version the replacement settled on.
func TestWithMethodReplacesInitialize(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("initialize", pinnedInitialize{version: "2025-06-18"}))
	d := &recordingDispatcher{}
	s.SetEventDispatcher(d.fn())

	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	result := decodeResult(t, res.Response)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("the replacement did not answer: %v", result)
	}
	if res.SessionID == "" {
		t.Fatal("a completed initialize must still assign a session id")
	}

	events := d.all()
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev, ok := events[0].(event.SessionInitialized)
	if !ok {
		t.Fatalf("unexpected event type %T", events[0])
	}
	if ev.ProtocolVersion != "2025-06-18" {
		t.Fatalf("event protocol version = %q, want the version the handler settled on", ev.ProtocolVersion)
	}
}

// failingMethod reports a protocol failure the way a handler is required to.
type failingMethod struct{}

func (failingMethod) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Invalid params: nope.")
}

// TestFailedInitializeEstablishesNothing asserts a handshake that ends in an
// error leaves no trace: no session id is handed to the transport and no
// SessionInitialized event is dispatched. A session minted for a handshake that
// failed would be a credential the client was never told about, and a listener
// would be told a client connected when none did.
func TestFailedInitializeEstablishesNothing(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("initialize", failingMethod{}))
	d := &recordingDispatcher{}
	s.SetEventDispatcher(d.fn())

	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)

	err := errorOf(t, res)
	if err.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
	}
	if res.SessionID != "" {
		t.Fatalf("session id = %q, want none for a failed handshake", res.SessionID)
	}
	if events := d.all(); len(events) != 0 {
		t.Fatalf("want no event for a failed handshake, got %v", events)
	}
	if len(res.Response.Result) != 0 {
		t.Fatalf("an error response must carry no result, got %s", res.Response.Result)
	}
}

// TestWithMethodErrorsAreNotEnveloped asserts a custom handler's error travels
// as an error response: it is not turned into a result, and the envelope adds
// nothing to it.
func TestWithMethodErrorsAreNotEnveloped(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithMethod("custom/fail", failingMethod{}))
	res := handle(t, s, modernRequest(1, "custom/fail"))

	err := errorOf(t, res)
	if err.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
	}
	if len(res.Response.Result) != 0 {
		t.Fatalf("an error response must carry no result, got %s", res.Response.Result)
	}
}
