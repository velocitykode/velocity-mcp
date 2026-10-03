package server_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// This file covers the one method that exists in the initialize handshake
// alone. Revision 2026-07-28 replaced initialize with server/discover and has
// no sessions (servers neither mint nor echo session ids), so a request that
// declares that revision in its _meta and calls initialize names a method its
// own protocol does not have. Serving it opened a session and minted an id on a
// stateless-era request.

// initializeBody is an initialize request whose params carry the given _meta
// members alongside the legacy negotiation members.
func initializeBody(meta string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` + meta +
		`"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
}

// sessionEvents returns a server whose SessionInitialized events are recorded,
// and a reader of the record.
func sessionEvents(s *server.Server) func() []event.SessionInitialized {
	var mu sync.Mutex
	var opened []event.SessionInitialized
	s.SetEventDispatcher(func(_ context.Context, e any) error {
		if si, ok := e.(event.SessionInitialized); ok {
			mu.Lock()
			opened = append(opened, si)
			mu.Unlock()
		}
		return nil
	})
	return func() []event.SessionInitialized {
		mu.Lock()
		defer mu.Unlock()
		return append([]event.SessionInitialized(nil), opened...)
	}
}

// TestInitializeUnderTheDiscoveryRevisionIsNotFound asserts an initialize that
// declares 2026-07-28 in its _meta is answered -32601, the code for a method
// the server does not have under the request's revision, and that no session is
// opened: no id is minted and no SessionInitialized event is dispatched. A
// request declaring the discovery metadata only in part is refused by the
// metadata rules first, and likewise opens nothing.
func TestInitializeUnderTheDiscoveryRevisionIsNotFound(t *testing.T) {
	tests := []struct {
		name     string
		meta     string
		wantCode int
		wantMsg  string
	}{
		{
			name:     "full discovery metadata",
			meta:     `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},`,
			wantCode: jsonrpc.CodeMethodNotFound,
			wantMsg:  "The method [initialize] was not found.",
		},
		{
			name:     "protocol version only",
			meta:     `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},`,
			wantCode: jsonrpc.CodeInvalidParams,
			wantMsg:  "Invalid params: The request [_meta] is missing the required [io.modelcontextprotocol/clientCapabilities] member.",
		},
		{
			name:     "client capabilities only",
			meta:     `"_meta":{"io.modelcontextprotocol/clientCapabilities":{}},`,
			wantCode: jsonrpc.CodeInvalidParams,
			wantMsg:  "Invalid params: The request [_meta] is missing the required [io.modelcontextprotocol/protocolVersion] member.",
		},
		{
			name:     "a version the server does not speak",
			meta:     `"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01","io.modelcontextprotocol/clientCapabilities":{}},`,
			wantCode: jsonrpc.CodeUnsupportedProtocolVersion,
			wantMsg:  "Unsupported protocol version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			opened := sessionEvents(s)
			res := s.Handle(context.Background(), []byte(initializeBody(tt.meta)), "")
			if !res.HasResponse || res.Response == nil || res.Response.Error == nil {
				t.Fatalf("response = %+v, want an error", res.Response)
			}
			if res.Response.Error.Code != tt.wantCode || res.Response.Error.Message != tt.wantMsg {
				t.Fatalf("error = %d %q, want %d %q", res.Response.Error.Code, res.Response.Error.Message, tt.wantCode, tt.wantMsg)
			}
			if res.SessionID != "" {
				t.Fatalf("a stateless-era request was given session %q", res.SessionID)
			}
			if got := opened(); len(got) != 0 {
				t.Fatalf("SessionInitialized dispatched for a stateless-era request: %+v", got)
			}
		})
	}
}

// TestInitializeUnderTheInitializeRevisionStillOpensASession asserts the
// control: an initialize carrying no discovery metadata (none at all, or a _meta
// holding only a progress token) negotiates, opens a session and reports it.
func TestInitializeUnderTheInitializeRevisionStillOpensASession(t *testing.T) {
	for _, meta := range []string{``, `"_meta":{"progressToken":1},`} {
		t.Run(meta, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			opened := sessionEvents(s)
			res := s.Handle(context.Background(), []byte(initializeBody(meta)), "")
			if res.Response == nil || res.Response.Error != nil {
				t.Fatalf("initialize refused: %+v", res.Response)
			}
			var result struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(res.Response.Result, &result); err != nil || result.ProtocolVersion != "2025-11-25" {
				t.Fatalf("result %s does not negotiate 2025-11-25", res.Response.Result)
			}
			if res.SessionID == "" || !s.IssuedSessionID(res.SessionID) {
				t.Fatalf("session id = %q, want one the server issued", res.SessionID)
			}
			if got := opened(); len(got) != 1 || got[0].SessionID != res.SessionID {
				t.Fatalf("SessionInitialized events = %+v, want one naming %q", got, res.SessionID)
			}
		})
	}
}
