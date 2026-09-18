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

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// negotiatingServer is a server that speaks the initialize handshake only: it
// answers initialize with settled and, like any server that does not know the
// method, rejects server/discover with a method-not-found. It records the
// version initialize was asked for, and the protocol version header carried by
// every request made once the handshake is over.
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

		handshaking := req.Method == "initialize" || req.Method == "server/discover"
		mu.Lock()
		if req.Method == "initialize" {
			requested = req.Params.ProtocolVersion
		}
		if !handshaking {
			headers = append(headers, r.Header.Get(protocolVersionHeader))
		}
		mu.Unlock()

		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "server/discover":
			envelope["error"] = map[string]any{"code": jsonrpc.CodeMethodNotFound, "message": "Method not found."}
		case "initialize":
			envelope["result"] = map[string]any{
				"protocolVersion": settled,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0.0"},
			}
		default:
			envelope["result"] = map[string]any{}
		}
		out, _ := json.Marshal(envelope)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	return srv, func() (string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return requested, append([]string(nil), headers...)
	}
}

// TestPingAsksWhatTheNegotiatedRevisionDefines asserts the liveness request the
// client puts on the wire is the one the settled revision defines. Revision
// 2026-07-28 removed the ping request, so a server conforming to it answers
// ping with a method-not-found while being perfectly healthy; what that
// revision requires every server to serve is server/discover. The servers here
// implement exactly their own revision, so asking for the other one's request
// fails the call.
func TestPingAsksWhatTheNegotiatedRevisionDefines(t *testing.T) {
	tests := []struct {
		name string
		// serve shapes the server into one of a single revision.
		serve func(*fakeTransport)
		// wantVersion is the revision the handshake settles on.
		wantVersion ProtocolVersion
		// wantFrames is every frame the client sends, handshake included.
		wantFrames []string
	}{
		{
			name: "the revision that removed ping is asked with server/discover",
			serve: func(f *fakeTransport) {
				f.without("ping")
				f.on("server/discover", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
					resp, _ := jsonrpc.NewResult(id, map[string]any{
						"supportedVersions": []any{LatestProtocolVersion},
						"capabilities":      map[string]any{},
					})
					return resp
				})
			},
			wantVersion: LatestProtocolVersion,
			wantFrames:  []string{"server/discover", "server/discover"},
		},
		{
			name:        "an initialize-era revision is asked with ping",
			serve:       func(f *fakeTransport) { f.without("server/discover") },
			wantVersion: server.InitializeSupportedVersions()[0],
			wantFrames:  []string{"server/discover", "initialize", "notifications/initialized", "ping"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTransport()
			tc.serve(f)
			c := newTestClient(f)

			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("ping: %v", err)
			}
			version, err := c.ProtocolVersion(context.Background())
			if err != nil {
				t.Fatalf("protocol version: %v", err)
			}
			if version != tc.wantVersion {
				t.Fatalf("negotiated version = %q, want %q", version, tc.wantVersion)
			}
			if got := sentMethods(f); !slices.Equal(got, tc.wantFrames) {
				t.Fatalf("frames = %v, want %v", got, tc.wantFrames)
			}
		})
	}
}

// TestPingIsNamedForTheConnectionItTravelsOver asserts the liveness request is
// chosen for the connection each attempt travels over, by the exchange that
// sends it. The first attempt finds the session gone, which renegotiates the
// connection under the request, and the server reached again speaks the other
// revision: the repeat is the request that revision defines, not the one chosen
// for the connection that was replaced, which the server now answering would
// refuse while being perfectly alive.
func TestPingIsNamedForTheConnectionItTravelsOver(t *testing.T) {
	tests := []struct {
		name string
		// frames is everything the two servers answer with, handshakes included.
		frames []string
		// expired is the liveness request whose first attempt finds the session
		// gone: the one the first connection's revision defines.
		expired     string
		wantMethods []string
		// wantVersion is the revision the last frame, the repeat, is sent under.
		wantVersion ProtocolVersion
	}{
		{
			name: "a server replaced by one of the revision that removed ping",
			frames: []string{
				methodNotFoundFrame(), initializeFrame(ProtocolV20251125),
				// The server reached again no longer answers initialize, and
				// does answer the probe.
				methodNotFoundFrame(), discoverFrame(LatestProtocolVersion),
				discoverFrame(LatestProtocolVersion),
			},
			expired: "ping",
			wantMethods: []string{
				"server/discover", "initialize", "notifications/initialized",
				"initialize", "server/discover", "server/discover",
			},
			wantVersion: LatestProtocolVersion,
		},
		{
			name: "a server replaced by one of a revision that defines ping",
			frames: []string{
				discoverFrame(LatestProtocolVersion),
				methodNotFoundFrame(), initializeFrame(ProtocolV20251125),
				resultFrame(map[string]any{}),
			},
			expired: "server/discover",
			wantMethods: []string{
				"server/discover",
				"server/discover", "initialize", "notifications/initialized", "ping",
			},
			wantVersion: ProtocolV20251125,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(tc.frames...)
			c := newScriptedClient(s)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			s.failOnce[tc.expired] = errSessionExpired

			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("ping: %v (methods = %v)", err, s.methods())
			}
			if got := s.methods(); !slices.Equal(got, tc.wantMethods) {
				t.Fatalf("methods = %v, want %v", got, tc.wantMethods)
			}
			announced := s.versionsAnnounced()
			if got := announced[len(announced)-1]; got != tc.wantVersion {
				t.Fatalf("the repeat was sent under %q, want %q", got, tc.wantVersion)
			}
		})
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
