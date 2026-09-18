package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/server"
)

// negotiatingServer answers initialize with settled and records, per method,
// the protocol version header the client sent with the request.
func negotiatingServer(t *testing.T, settled string) (*httptest.Server, func() (string, []string)) {
	t.Helper()

	var mu sync.Mutex
	var requested string
	var headers []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		if req.Method == "initialize" {
			requested = req.Params.ProtocolVersion
		} else {
			headers = append(headers, r.Header.Get(protocolVersionHeader))
		}
		mu.Unlock()

		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		result := map[string]any{}
		if req.Method == "initialize" {
			result = map[string]any{
				"protocolVersion": settled,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0.0"},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	return srv, func() (string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return requested, append([]string(nil), headers...)
	}
}

// TestInitializeAsksForALegacyVersion asserts the client opens the legacy
// handshake with a version that handshake actually negotiates over. Asking for
// the newest revision would be asking a server to settle on one it only offers
// through the discovery handshake.
func TestInitializeAsksForALegacyVersion(t *testing.T) {
	offered := server.InitializeSupportedVersions()
	srv, recorded := negotiatingServer(t, offered[0])

	c := Web(srv.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer c.Disconnect()

	requested, _ := recorded()
	if !slices.Contains(offered, requested) {
		t.Fatalf("initialize asked for %q, which the legacy handshake offers none of (%v)", requested, offered)
	}
	if requested == server.LatestProtocolVersion {
		t.Fatalf("initialize asked for %q, which is negotiated through the discovery handshake, not this one", requested)
	}
}

// TestProtocolVersionHeaderIsTheNegotiatedVersion asserts every request made
// after the handshake advertises the version the server settled on, not the
// newest one this package knows. A server that negotiated an older revision
// answers 400 to a header naming a version it does not speak.
func TestProtocolVersionHeaderIsTheNegotiatedVersion(t *testing.T) {
	const settled = server.ProtocolV20250618
	if settled == server.LatestProtocolVersion {
		t.Fatalf("the test needs a settled version distinct from the newest one")
	}
	srv, recorded := negotiatingServer(t, settled)

	c := Web(srv.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer c.Disconnect()

	_, headers := recorded()
	if len(headers) == 0 {
		t.Fatal("no post-handshake request was observed")
	}
	for _, got := range headers {
		if got != settled {
			t.Fatalf("%s = %q, want the negotiated %q", protocolVersionHeader, got, settled)
		}
	}
}
