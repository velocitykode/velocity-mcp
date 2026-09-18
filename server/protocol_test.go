package server_test

import (
	"slices"
	"testing"

	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/server"
)

// TestHandshakeForEveryVersion pins the handshake each revision uses. The split
// is what decides whether a request must carry protocol metadata, so a version
// silently landing in the wrong family would change the validation rules for
// every request made under it.
func TestHandshakeForEveryVersion(t *testing.T) {
	tests := []struct {
		version string
		want    server.ProtocolHandshake
	}{
		{server.ProtocolV20260728, server.HandshakeDiscovery},
		{server.ProtocolV20251125, server.HandshakeInitialize},
		{server.ProtocolV20250618, server.HandshakeInitialize},
		{server.ProtocolV20250326, server.HandshakeInitialize},
		{server.ProtocolV20241105, server.HandshakeInitialize},
		{"2099-01-01", server.HandshakeInitialize},
		{"", server.HandshakeInitialize},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := server.HandshakeFor(tt.version); got != tt.want {
				t.Fatalf("HandshakeFor(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestProtocolHandshakeString(t *testing.T) {
	tests := []struct {
		handshake server.ProtocolHandshake
		want      string
	}{
		{server.HandshakeDiscovery, "discovery"},
		{server.HandshakeInitialize, "initialize"},
		{server.ProtocolHandshake(42), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.handshake.String(); got != tt.want {
			t.Fatalf("String() = %q, want %q", got, tt.want)
		}
	}
}

// TestVersionSetsPerFlow pins the two version lists. They are deliberately
// disjoint: the advertised set is the current revision, and the initialize
// handshake offers only the revisions that predate discovery.
func TestVersionSetsPerFlow(t *testing.T) {
	if got := server.ServerSupportedVersions(); !slices.Equal(got, []string{"2026-07-28"}) {
		t.Fatalf("ServerSupportedVersions() = %v", got)
	}
	if got := server.InitializeSupportedVersions(); !slices.Equal(got, []string{"2025-11-25", "2025-06-18"}) {
		t.Fatalf("InitializeSupportedVersions() = %v", got)
	}
	if server.LatestProtocolVersion != server.ProtocolV20260728 {
		t.Fatalf("LatestProtocolVersion = %q", server.LatestProtocolVersion)
	}
	for _, v := range server.InitializeSupportedVersions() {
		if server.HandshakeFor(v) != server.HandshakeInitialize {
			t.Fatalf("version %q offered by initialize but uses another handshake", v)
		}
		if slices.Contains(server.ServerSupportedVersions(), v) {
			t.Fatalf("version %q appears in both the advertised and the initialize sets", v)
		}
	}
}

// TestVersionListsAreCopies asserts the version accessors hand out fresh
// slices: a caller mutating one must not reshape what the next caller sees.
func TestVersionListsAreCopies(t *testing.T) {
	first := server.ServerSupportedVersions()
	first[0] = "tampered"
	if got := server.ServerSupportedVersions()[0]; got != server.ProtocolV20260728 {
		t.Fatalf("ServerSupportedVersions() leaked shared state: %q", got)
	}

	offered := server.InitializeSupportedVersions()
	offered[0] = "tampered"
	if got := server.InitializeSupportedVersions()[0]; got != server.ProtocolV20251125 {
		t.Fatalf("InitializeSupportedVersions() leaked shared state: %q", got)
	}
}

// TestInitializeNegotiation covers the legacy handshake end to end: the version
// a client asks for is echoed when the handshake offers it, and every other
// request (unknown, ill-typed, or absent) is answered with the newest offered
// version rather than an error.
func TestInitializeNegotiation(t *testing.T) {
	tests := []struct {
		name   string
		params string
		want   string
	}{
		{"offered version is echoed", `{"protocolVersion":"2025-06-18"}`, "2025-06-18"},
		{"newest offered version is echoed", `{"protocolVersion":"2025-11-25"}`, "2025-11-25"},
		{"retired version falls back", `{"protocolVersion":"2025-03-26"}`, "2025-11-25"},
		{"oldest version falls back", `{"protocolVersion":"2024-11-05"}`, "2025-11-25"},
		{"discovery revision falls back", `{"protocolVersion":"2026-07-28"}`, "2025-11-25"},
		{"unknown version falls back", `{"protocolVersion":"1999-01-01"}`, "2025-11-25"},
		{"non-string version falls back", `{"protocolVersion":123}`, "2025-11-25"},
		{"null version falls back", `{"protocolVersion":null}`, "2025-11-25"},
		{"absent version falls back", `{}`, "2025-11-25"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			d := &recordingDispatcher{}
			s.SetEventDispatcher(d.fn())

			res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+tt.params+`}`)
			result := decodeResult(t, res.Response)
			if result["protocolVersion"] != tt.want {
				t.Fatalf("protocolVersion = %v, want %q", result["protocolVersion"], tt.want)
			}
			if res.SessionID == "" {
				t.Fatal("a completed initialize must assign a session id")
			}

			// The session event reports the version the connection will run
			// under, which is the one on the wire, not the one the client asked
			// for: a listener that pins behaviour to a revision would otherwise
			// act on a version neither side agreed to.
			events := d.all()
			if len(events) != 1 {
				t.Fatalf("want 1 event, got %d", len(events))
			}
			ev, ok := events[0].(event.SessionInitialized)
			if !ok {
				t.Fatalf("unexpected event type %T", events[0])
			}
			if ev.ProtocolVersion != tt.want {
				t.Fatalf("event protocol version = %q, want the negotiated %q", ev.ProtocolVersion, tt.want)
			}
			if ev.SessionID != res.SessionID {
				t.Fatalf("event session id = %q, want %q", ev.SessionID, res.SessionID)
			}
		})
	}
}
