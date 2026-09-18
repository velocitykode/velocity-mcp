package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// testClientInfo is the identity the negotiation tests send to servers.
func testClientInfo() schema.Implementation {
	return schema.NewImplementation("Acme MCP App", "9.9.9")
}

func newScriptedClient(s *scriptedTransport) *Client {
	return New(s, testClientInfo())
}

func TestHandshakeNegotiation(t *testing.T) {
	tests := []struct {
		name string
		// pin is the protocol version the client offers, or empty to negotiate.
		pin ProtocolVersion
		// script is the sequence of frames the server answers with.
		script []string
		// wantMethods is the exact sequence of methods the client sends.
		wantMethods []string
		wantVersion ProtocolVersion
		// wantDiscoverResult asserts the connection was settled by discovery.
		wantDiscoverResult bool
		// wantErr is a substring of the error the handshake must fail with.
		wantErr string
		// wantErrCode is the JSON-RPC code the failure must carry, when the
		// failure comes from the server.
		wantErrCode int
	}{
		{
			name:               "discovery server settles on the latest version",
			script:             []string{discoverFrame(LatestProtocolVersion)},
			wantMethods:        []string{"server/discover"},
			wantVersion:        LatestProtocolVersion,
			wantDiscoverResult: true,
		},
		{
			name:        "a server without the discovery method falls back to initialize",
			script:      []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			// The fallback offers 2025-11-25 without pinning it, so a server that
			// only speaks 2025-06-18 settles the connection there and the client
			// speaks that version from then on.
			name:        "a legacy server settles the unpinned fallback on an older version",
			script:      []string{methodNotFoundFrame(), initializeFrame(ProtocolV20250618)},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20250618,
		},
		{
			name: "an implementation defined rejection falls back to initialize",
			script: []string{
				errorFrame(-32000, "Bad Request: Unsupported protocol version: 2026-07-28", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "an uncorrelated rejection falls back to initialize",
			script: []string{
				nullIDErrorFrame(-32000, "Bad Request: Unsupported protocol version: 2026-07-28"),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "a parse error falls back to initialize",
			script: []string{
				errorFrame(jsonrpc.CodeParseError, "Parse error", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "an invalid request falls back to initialize",
			script: []string{
				errorFrame(jsonrpc.CodeInvalidRequest, "Invalid Request", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "the oldest implementation defined code still falls back to initialize",
			script: []string{
				errorFrame(-32099, "Bad Request", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			// The fallback is bound to no single code: a server that predates
			// the discovery handshake answers an unknown pre-initialize request
			// with whatever its framework returns, inside the reserved range or
			// outside it.
			name: "a code below the implementation defined range falls back to initialize",
			script: []string{
				errorFrame(-32100, "Reserved", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "a code above the implementation defined range falls back to initialize",
			script: []string{
				errorFrame(-31999, "Application error", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "a discovery era code inside the implementation defined range is not read as legacy",
			// -32022 sits inside the range a legacy server rejects with, so the
			// probe only gets this right by classifying the discovery codes first.
			script:      []string{errorFrame(CodeUnsupportedProtocolVersion, "Unsupported protocol version", nil)},
			wantMethods: []string{"server/discover"},
			wantErr:     "Unsupported protocol version",
			wantErrCode: CodeUnsupportedProtocolVersion,
		},
		{
			name: "an internal error falls back to initialize",
			script: []string{
				errorFrame(jsonrpc.CodeInternalError, "Internal error", nil),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			// Invalid params is one of the two codes the specification names as
			// a common answer from a legacy server, alongside method-not-found.
			name: "an invalid params error falls back to initialize",
			script: []string{
				errorFrame(jsonrpc.CodeInvalidParams, "Invalid params: the request [_meta] is malformed.", nil),
				initializeFrame(ProtocolV20250618),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20250618,
		},
		{
			// The fallback carries the whole story when it fails too: the
			// rejection that sent the client looking for a legacy handshake and
			// the failure of that handshake.
			name: "a rejected probe and a failing fallback are reported together",
			script: []string{
				errorFrame(jsonrpc.CodeInvalidParams, "Invalid params: the request [_meta] is malformed.", nil),
				errorFrame(jsonrpc.CodeInternalError, "Internal error", nil),
			},
			wantMethods: []string{"server/discover", "initialize"},
			wantErr:     "the request [_meta] is malformed.; the legacy handshake also failed: jsonrpc: code -32603: Internal error",
			wantErrCode: jsonrpc.CodeInternalError,
		},
		{
			name:        "a header mismatch identifies a discovery server and is surfaced",
			script:      []string{errorFrame(CodeHeaderMismatch, "Header mismatch: The [Mcp-Method] header is required.", nil)},
			wantMethods: []string{"server/discover"},
			wantErr:     "Header mismatch",
			wantErrCode: CodeHeaderMismatch,
		},
		{
			name: "a missing client capability identifies a discovery server and is surfaced",
			script: []string{errorFrame(CodeMissingRequiredClientCapability,
				"The [elicitation] client capability is required.", nil)},
			wantMethods: []string{"server/discover"},
			wantErr:     "client capability is required",
			wantErrCode: CodeMissingRequiredClientCapability,
		},
		{
			name: "an unsupported version is negotiated down to a mutual version",
			script: []string{
				errorFrame(CodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
					"supported": []string{ProtocolV20251125, ProtocolV20250618},
					"requested": LatestProtocolVersion,
				}),
				initializeFrame(ProtocolV20251125),
			},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name: "an unsupported version with no mutual version is surfaced",
			script: []string{
				errorFrame(CodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
					"supported": []string{"2027-01-01"},
					"requested": LatestProtocolVersion,
				}),
			},
			wantMethods: []string{"server/discover"},
			wantErr:     "Unsupported protocol version",
			wantErrCode: CodeUnsupportedProtocolVersion,
		},
		{
			// The rejection names a version the initialize handshake never
			// settles, so there is nothing to negotiate down to: offering it
			// through initialize would put a shape on the wire neither peer
			// agreed on.
			name: "an unsupported version advertising only a discovery era version is surfaced",
			script: []string{
				errorFrame(CodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
					"supported": []string{LatestProtocolVersion},
					"requested": LatestProtocolVersion,
				}),
			},
			wantMethods: []string{"server/discover"},
			wantErr:     "Unsupported protocol version",
			wantErrCode: CodeUnsupportedProtocolVersion,
		},
		{
			name:        "discovery advertising only a legacy version settles through initialize",
			script:      []string{discoverFrame(ProtocolV20251125), initializeFrame(ProtocolV20251125)},
			wantMethods: []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name:        "a server with no mutually supported version is refused",
			script:      []string{discoverFrame("2027-01-01")},
			wantMethods: []string{"server/discover"},
			wantErr: "the server supports protocol versions [2027-01-01]; this client supports " +
				"[2026-07-28, 2025-11-25, 2025-06-18]",
		},
		{
			name:        "a malformed discover result is refused",
			script:      []string{resultFrame(map[string]any{"capabilities": map[string]any{}})},
			wantMethods: []string{"server/discover"},
			wantErr:     "invalid discover response from server",
		},
		{
			name:        "a version the client cannot speak through initialize is refused",
			script:      []string{methodNotFoundFrame(), initializeFrame("2025-03-26")},
			wantMethods: []string{"server/discover", "initialize"},
			wantErr:     "the server chose protocol version [2025-03-26]; this client supports [2025-11-25, 2025-06-18]",
		},
		{
			name:        "a failing fallback surfaces the handshake error",
			script:      []string{methodNotFoundFrame(), errorFrame(jsonrpc.CodeInternalError, "Internal error", nil)},
			wantMethods: []string{"server/discover", "initialize"},
			wantErr:     "Internal error",
			wantErrCode: jsonrpc.CodeInternalError,
		},
		{
			name:               "a pinned discovery version skips the probe",
			pin:                LatestProtocolVersion,
			script:             []string{discoverFrame(LatestProtocolVersion)},
			wantMethods:        []string{"server/discover"},
			wantVersion:        LatestProtocolVersion,
			wantDiscoverResult: true,
		},
		{
			name:        "a pinned legacy version skips the probe",
			pin:         ProtocolV20250618,
			script:      []string{initializeFrame(ProtocolV20250618)},
			wantMethods: []string{"initialize", "notifications/initialized"},
			wantVersion: ProtocolV20250618,
		},
		{
			name:        "a server that will not settle on the pinned discovery version is refused",
			pin:         LatestProtocolVersion,
			script:      []string{discoverFrame(ProtocolV20251125)},
			wantMethods: []string{"server/discover"},
			wantErr:     "the server settled on protocol version [2025-11-25] while [2026-07-28] was requested",
		},
		{
			name:        "a server that will not settle on the pinned legacy version is refused",
			pin:         ProtocolV20251125,
			script:      []string{initializeFrame(ProtocolV20250618)},
			wantMethods: []string{"initialize"},
			wantErr:     "the server settled on protocol version [2025-06-18] while [2025-11-25] was requested",
		},
		{
			name:        "a version this client does not speak cannot be pinned",
			pin:         "2024-11-05",
			script:      []string{discoverFrame(LatestProtocolVersion)},
			wantMethods: []string{},
			wantErr: "this client does not support protocol version [2024-11-05]; it supports " +
				"[2026-07-28, 2025-11-25, 2025-06-18]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(tc.script...)
			c := newScriptedClient(s)
			if tc.pin != "" {
				c.WithProtocolVersion(tc.pin)
			}

			err := c.Connect(context.Background())

			if got := s.methods(); !slices.Equal(got, tc.wantMethods) {
				t.Fatalf("sent methods = %v, want %v", got, tc.wantMethods)
			}

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected handshake to fail with %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if tc.wantErrCode != 0 {
					var rpcErr *jsonrpc.Error
					if !errors.As(err, &rpcErr) {
						t.Fatalf("error %v is not a JSON-RPC error", err)
					}
					if rpcErr.Code != tc.wantErrCode {
						t.Fatalf("error code = %d, want %d", rpcErr.Code, tc.wantErrCode)
					}
				}
				if c.Connected() {
					t.Fatal("client reports a connection after a failed handshake")
				}
				return
			}

			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			version, err := c.ProtocolVersion(context.Background())
			if err != nil {
				t.Fatalf("protocol version: %v", err)
			}
			if version != tc.wantVersion {
				t.Fatalf("negotiated version = %q, want %q", version, tc.wantVersion)
			}
			if (c.DiscoverResult() != nil) != tc.wantDiscoverResult {
				t.Fatalf("discover result present = %v, want %v", c.DiscoverResult() != nil, tc.wantDiscoverResult)
			}
			if (c.InitializeResult() != nil) == tc.wantDiscoverResult {
				t.Fatalf("initialize result present = %v, want %v", c.InitializeResult() != nil, !tc.wantDiscoverResult)
			}
		})
	}
}

func TestHandshakeFallsBackWhenTheProbeTimesOut(t *testing.T) {
	s := newScriptedTransport(initializeFrame(ProtocolV20251125))
	s.failOn["server/discover"] = NewTimeoutError("timed out while waiting for server response", nil)

	c := newScriptedClient(s)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if got := s.methods(); !slices.Equal(got, []string{"initialize", "notifications/initialized"}) {
		t.Fatalf("sent methods = %v", got)
	}
	if _, _, connected := s.lifecycle(); !connected {
		t.Fatal("the transport was left disconnected after the fallback")
	}
	if c.DiscoverResult() != nil {
		t.Fatal("a timed-out probe must not leave a discover result behind")
	}
}

func TestHandshakeReportsBothHalvesWhenTheFallbackAlsoFails(t *testing.T) {
	s := newScriptedTransport(errorFrame(jsonrpc.CodeInternalError, "Internal error", nil))
	s.failOn["server/discover"] = NewTransportError(
		"the endpoint [https://mcp.test/mcp] rejected the request with HTTP status [405]", nil)

	c := newScriptedClient(s)
	err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected the handshake to fail")
	}

	want := "the endpoint [https://mcp.test/mcp] rejected the request with HTTP status [405]; " +
		"the legacy handshake also failed: jsonrpc: code -32603: Internal error"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInternalError {
		t.Fatalf("the handshake failure must still carry the server error, got %v", err)
	}
}

// TestAnUnpinnedFallbackSpeaksTheVersionTheServerSettledOn pins that the
// offered version and the settled one are kept apart on the ordinary path
// against a server that only speaks 2025-06-18: the fallback offers 2025-11-25
// unpinned, the server answers with the older version, and every frame from the
// initialized notification on belongs to the version that was settled.
func TestAnUnpinnedFallbackSpeaksTheVersionTheServerSettledOn(t *testing.T) {
	s := newScriptedTransport(
		methodNotFoundFrame(),
		initializeFrame(ProtocolV20250618),
		resultFrame(map[string]any{"tools": []any{}}),
	)
	c := newScriptedClient(s)

	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	wantMethods := []string{"server/discover", "initialize", "notifications/initialized", "tools/list"}
	if got := s.methods(); !slices.Equal(got, wantMethods) {
		t.Fatalf("sent methods = %v, want %v", got, wantMethods)
	}
	if got := s.frame(t, 1).Params["protocolVersion"]; got != ProtocolV20251125 {
		t.Fatalf("offered protocol version = %v, want the unpinned %q", got, ProtocolV20251125)
	}

	// The probe offers the latest, the fallback offers 2025-11-25, and the
	// notification and the request that follows speak what the server settled on.
	wantVersions := []ProtocolVersion{
		LatestProtocolVersion, ProtocolV20251125, ProtocolV20250618, ProtocolV20250618,
	}
	if got := s.versionsAnnounced(); !slices.Equal(got, wantVersions) {
		t.Fatalf("announced versions = %v, want %v", got, wantVersions)
	}

	version, err := c.ProtocolVersion(context.Background())
	if err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if version != ProtocolV20250618 {
		t.Fatalf("negotiated version = %q, want %q", version, ProtocolV20250618)
	}
	if result := c.InitializeResult(); result == nil || result.ProtocolVersion != ProtocolV20250618 {
		t.Fatalf("initialize result = %+v, want the settled version", result)
	}
}

func TestHandshakeKeepsAnAuthorizationFailureIntact(t *testing.T) {
	authErr := &oauth.AuthorizationRequiredError{Message: "authorization is required"}
	s := newScriptedTransport()
	s.failOn["server/discover"] = NewTransportError("the endpoint rejected the request with HTTP status [405]", nil)
	s.failOn["initialize"] = authErr

	err := newScriptedClient(s).Connect(context.Background())

	// The authorization failure travels out untouched: the probe rejection is
	// not folded into it, because credentials, not the protocol, are what is
	// missing and no wording about handshakes would help the caller.
	if err != error(authErr) {
		t.Fatalf("error = %#v, want the authorization failure itself", err)
	}
	if err.Error() != "authorization is required" {
		t.Fatalf("error = %q, want %q", err.Error(), "authorization is required")
	}
	var clientErr *Error
	if errors.As(err, &clientErr) {
		t.Fatalf("the authorization failure was wrapped in a client error: %q", clientErr.Error())
	}
}

func TestLegacyHandshakeCarriesNoDiscoveryMetadata(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	initialize := s.frame(t, 1)
	if initialize.Method != "initialize" {
		t.Fatalf("frame 1 = %q, want initialize", initialize.Method)
	}
	if got := initialize.Params["protocolVersion"]; got != ProtocolV20251125 {
		t.Fatalf("offered protocol version = %v, want %q", got, ProtocolV20251125)
	}
	if _, ok := initialize.Params["_meta"]; ok {
		t.Fatalf("the initialize request must carry no protocol metadata, got %v", initialize.Params["_meta"])
	}
	if info, ok := initialize.Params["clientInfo"].(map[string]any); !ok || info["name"] != "Acme MCP App" {
		t.Fatalf("clientInfo = %v", initialize.Params["clientInfo"])
	}
	if headers := s.headersAt(1); len(headers) != 0 {
		t.Fatalf("a legacy request must carry no protocol headers, got %v", headers)
	}
}

func TestDiscoveryEraRequestsCarryProtocolMetadata(t *testing.T) {
	s := newScriptedTransport(
		discoverFrame(LatestProtocolVersion),
		resultFrame(map[string]any{"tools": []any{map[string]any{"name": "add"}}}),
	)
	c := newScriptedClient(s)

	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if got := s.methods(); !slices.Equal(got, []string{"server/discover", "tools/list"}) {
		t.Fatalf("sent methods = %v", got)
	}

	for index := range 2 {
		meta := s.frame(t, index).meta()
		if meta == nil {
			t.Fatalf("frame %d carries no protocol metadata", index)
		}
		if meta[MetaProtocolVersion] != LatestProtocolVersion {
			t.Fatalf("frame %d protocol version = %v, want %q", index, meta[MetaProtocolVersion], LatestProtocolVersion)
		}
		capabilities, ok := meta[MetaClientCapabilities].(map[string]any)
		if !ok {
			t.Fatalf("frame %d client capabilities = %v, want an object", index, meta[MetaClientCapabilities])
		}
		if len(capabilities) != 0 {
			t.Fatalf("frame %d client capabilities = %v, want an empty object", index, capabilities)
		}
		info, ok := meta[MetaClientInfo].(map[string]any)
		if !ok || info["name"] != "Acme MCP App" || info["version"] != "9.9.9" {
			t.Fatalf("frame %d client info = %v", index, meta[MetaClientInfo])
		}
	}
}

func TestDiscoveryEraMirrorsRequestHeaders(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
		// prelude answers whatever the call reads before the request itself.
		prelude     []string
		reply       string
		wantMethod  string
		wantHeaders map[string]string
	}{
		{
			name:    "a tool call mirrors the method and the tool name",
			prelude: []string{emptyToolsFrame()},
			call: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"query": "select 1"})
				return err
			},
			reply:       resultFrame(map[string]any{"content": []any{}, "isError": false}),
			wantMethod:  "tools/call",
			wantHeaders: map[string]string{methodHeader: "tools/call", nameHeader: "execute_sql"},
		},
		{
			name: "a prompt fetch mirrors the method and the prompt name",
			call: func(c *Client) error {
				_, err := c.GetPrompt(context.Background(), "greeting", nil)
				return err
			},
			reply:       resultFrame(map[string]any{"messages": []any{}}),
			wantMethod:  "prompts/get",
			wantHeaders: map[string]string{methodHeader: "prompts/get", nameHeader: "greeting"},
		},
		{
			name: "a resource read mirrors the method and the uri",
			call: func(c *Client) error {
				_, err := c.ReadResource(context.Background(), "file:///notes.txt")
				return err
			},
			reply:       resultFrame(map[string]any{"contents": []any{}}),
			wantMethod:  "resources/read",
			wantHeaders: map[string]string{methodHeader: "resources/read", nameHeader: "file:///notes.txt"},
		},
		{
			name: "a listing mirrors the method alone",
			call: func(c *Client) error {
				_, err := c.Tools(context.Background())
				return err
			},
			reply:       resultFrame(map[string]any{"tools": []any{}}),
			wantMethod:  "tools/list",
			wantHeaders: map[string]string{methodHeader: "tools/list"},
		},
		{
			name: "a name that cannot travel verbatim is encoded",
			call: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "hé\r\nX-Injected: 1", nil)
				return err
			},
			prelude:    []string{emptyToolsFrame()},
			reply:      resultFrame(map[string]any{"content": []any{}, "isError": false}),
			wantMethod: "tools/call",
			wantHeaders: map[string]string{
				methodHeader: "tools/call",
				nameHeader:   "=?base64?" + "aMOpDQpYLUluamVjdGVkOiAx" + "?=",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := append([]string{discoverFrame(LatestProtocolVersion)}, tc.prelude...)
			s := newScriptedTransport(append(script, tc.reply)...)
			c := newScriptedClient(s)

			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			index := len(tc.prelude) + 1
			if got := s.frame(t, index).Method; got != tc.wantMethod {
				t.Fatalf("frame %d method = %q, want %q", index, got, tc.wantMethod)
			}

			headers := s.headersAt(index)
			if len(headers) != len(tc.wantHeaders) {
				t.Fatalf("headers = %v, want %v", headers, tc.wantHeaders)
			}
			for name, want := range tc.wantHeaders {
				if headers[name] != want {
					t.Fatalf("header %s = %q, want %q", name, headers[name], want)
				}
			}
			// The discovery handshake announces its version to the transport on
			// every frame it sends.
			for _, version := range s.versionsAnnounced() {
				if version != LatestProtocolVersion {
					t.Fatalf("announced protocol version = %q, want %q", version, LatestProtocolVersion)
				}
			}
		})
	}
}

func TestReconnectRemembersTheNegotiatedHandshake(t *testing.T) {
	tests := []struct {
		name string
		// first and second are the scripts for the two connections.
		first       []string
		second      []string
		wantSecond  []string
		wantVersion ProtocolVersion
	}{
		{
			name:        "a discovery server is probed with discovery again",
			first:       []string{discoverFrame(LatestProtocolVersion)},
			second:      []string{discoverFrame(LatestProtocolVersion)},
			wantSecond:  []string{"server/discover"},
			wantVersion: LatestProtocolVersion,
		},
		{
			name:        "a legacy server goes straight back to initialize",
			first:       []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)},
			second:      []string{initializeFrame(ProtocolV20251125)},
			wantSecond:  []string{"initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name:        "a discovery server that has turned legacy is re-probed",
			first:       []string{discoverFrame(LatestProtocolVersion)},
			second:      []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)},
			wantSecond:  []string{"server/discover", "initialize", "notifications/initialized"},
			wantVersion: ProtocolV20251125,
		},
		{
			name:        "a legacy server that has turned modern is re-probed",
			first:       []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)},
			second:      []string{methodNotFoundFrame(), discoverFrame(LatestProtocolVersion)},
			wantSecond:  []string{"initialize", "server/discover"},
			wantVersion: LatestProtocolVersion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(tc.first...)
			c := newScriptedClient(s)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("first connect: %v", err)
			}
			c.Disconnect()

			s.script(tc.second...)

			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("second connect: %v", err)
			}
			if got := s.methods(); !slices.Equal(got, tc.wantSecond) {
				t.Fatalf("second handshake sent %v, want %v", got, tc.wantSecond)
			}
			version, err := c.ProtocolVersion(context.Background())
			if err != nil {
				t.Fatalf("protocol version: %v", err)
			}
			if version != tc.wantVersion {
				t.Fatalf("negotiated version = %q, want %q", version, tc.wantVersion)
			}
		})
	}
}

func TestRememberedLegacyServerRethrowsAuthorizationFailure(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	c.Disconnect()

	s.mu.Lock()
	s.sent = nil
	s.sendErr = &oauth.AuthorizationRequiredError{Message: "authorization is required"}
	s.mu.Unlock()

	err := c.Connect(context.Background())
	var authErr *oauth.AuthorizationRequiredError
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %v, want an authorization failure", err)
	}
	// An expired token must not send the client back to the probe.
	if got := s.methods(); len(got) != 0 {
		t.Fatalf("frames sent after the authorization failure = %v", got)
	}
}

// TestAnAbandonedReconnectKeepsWhatWasNegotiated pins the context guard on the
// remembered-legacy path: a caller withdrawing the request ends the reconnect
// where it failed rather than spending a fresh channel on a probe that cannot
// travel either, and the handshake the server is known to speak is not
// forgotten over it.
func TestAnAbandonedReconnectKeepsWhatWasNegotiated(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	c.Disconnect()

	connects, _, _ := s.lifecycle()
	s.script()
	s.failOn["initialize"] = NewTransportError("the wait was cancelled", nil)

	// The caller withdraws while the remembered handshake is on the wire, which
	// is when a real one is abandoned: the channel then fails, and what matters
	// is that nothing goes looking for a second handshake over a dead context.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.beforeSend = func(method string) {
		if method == "initialize" {
			cancel()
		}
	}

	err := c.Connect(ctx)

	if err == nil {
		t.Fatal("expected the reconnect to fail")
	}
	if err.Error() != "the wait was cancelled" {
		t.Fatalf("error = %q, want the rejection of the remembered handshake alone", err.Error())
	}
	if got := s.methods(); len(got) != 0 {
		t.Fatalf("frames sent over a cancelled context = %v, want none", got)
	}
	if got, _, _ := s.lifecycle(); got != connects+1 {
		t.Fatalf("transport connects = %d, want %d: the abandoned reconnect opened a channel for a second handshake",
			got, connects+1)
	}

	// Nothing was forgotten: the next reconnect goes straight back to the
	// handshake this server answered, without probing for discovery again.
	delete(s.failOn, "initialize")
	s.script(initializeFrame(ProtocolV20251125))
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if got := s.methods(); !slices.Equal(got, []string{"initialize", "notifications/initialized"}) {
		t.Fatalf("second handshake sent %v, want the remembered initialize", got)
	}
}

// TestPinningForgetsTheNegotiatedConnection pins that pinning (and unpinning)
// discards what was negotiated: the results go with it straight away, and the
// next connect negotiates from scratch rather than replaying a handshake that
// belonged to the old pin.
func TestPinningForgetsTheNegotiatedConnection(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if c.InitializeResult() == nil {
		t.Fatal("the legacy handshake left no initialize result")
	}

	c.WithProtocolVersion(ProtocolV20250618)
	if c.InitializeResult() != nil || c.DiscoverResult() != nil {
		t.Fatalf("pinning kept the negotiated connection: initialize=%+v discover=%+v",
			c.InitializeResult(), c.DiscoverResult())
	}

	// An empty version clears the pin and restores negotiation. With nothing
	// remembered, the next connect goes back through the probe.
	c.WithProtocolVersion("")
	if c.InitializeResult() != nil || c.DiscoverResult() != nil {
		t.Fatalf("clearing the pin kept the negotiated connection: initialize=%+v discover=%+v",
			c.InitializeResult(), c.DiscoverResult())
	}

	s.script(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	wantMethods := []string{"server/discover", "initialize", "notifications/initialized"}
	if got := s.methods(); !slices.Equal(got, wantMethods) {
		t.Fatalf("handshake after unpinning sent %v, want %v", got, wantMethods)
	}
	version, err := c.ProtocolVersion(context.Background())
	if err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if version != ProtocolV20251125 {
		t.Fatalf("negotiated version = %q, want %q", version, ProtocolV20251125)
	}
}

func TestPinningAfterConnectingNegotiatesAgain(t *testing.T) {
	s := newScriptedTransport(
		methodNotFoundFrame(),
		initializeFrame(ProtocolV20251125),
		discoverFrame(LatestProtocolVersion),
		resultFrame(map[string]any{"tools": []any{}}),
	)
	c := newScriptedClient(s)

	version, err := c.ProtocolVersion(context.Background())
	if err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if version != ProtocolV20251125 {
		t.Fatalf("negotiated version = %q, want %q", version, ProtocolV20251125)
	}

	c.WithProtocolVersion(LatestProtocolVersion)
	if c.Connected() {
		t.Fatal("pinning a version must drop the connection")
	}
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	if got := s.methods(); !slices.Equal(got, []string{"server/discover", "initialize", "notifications/initialized", "server/discover", "tools/list"}) {
		t.Fatalf("sent methods = %v", got)
	}
	if meta := s.frame(t, 4).meta(); meta == nil || meta[MetaProtocolVersion] != LatestProtocolVersion {
		t.Fatalf("the request after re-pinning carries %v", meta)
	}
}

// TestWhatACallLearnsOfItsConnectionIsReadBeforeItLetsGo asserts a call reads
// the connection it settled while it still holds the exchange. Pinning a
// version discards the negotiated connection and waits its turn at the exchange
// to do it, so the moment a call's handshake lets go is the moment a pin waiting
// behind it goes through. A call that settled a connection and only then asked
// the protocol which one stands is told there is none, and reports a client
// that has not negotiated a version from the very call that negotiated one.
// The version, the liveness check, and a tool call by name all begin with that
// question, and are driven here while another goroutine keeps pinning, under
// the race detector.
func TestWhatACallLearnsOfItsConnectionIsReadBeforeItLetsGo(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		result := map[string]any{"resultType": "complete"}
		switch request.method {
		case "server/discover":
			result = map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{},
				"ttlMs":             3600000,
				"cacheScope":        "private",
			}
		case "tools/list":
			result["tools"] = []any{plainTool("execute_sql")}
			result["ttlMs"] = 3600000
			result["cacheScope"] = "private"
		case "tools/call":
			result["content"] = []any{map[string]any{"type": "text", "text": "done"}}
			result["isError"] = false
		}
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": result}))
	})
	client := Web(endpoint.URL)
	ctx := context.Background()

	calls := []struct {
		name string
		call func() error
	}{
		{name: "the version", call: func() error {
			version, err := client.ProtocolVersion(ctx)
			if err == nil && version != LatestProtocolVersion {
				return newError("version = " + version)
			}
			return err
		}},
		{name: "the liveness check", call: func() error { return client.Ping(ctx) }},
		{name: "a tool call by name", call: func() error {
			_, err := client.CallTool(ctx, "execute_sql", nil)
			// A call whose connection was replaced under it once too often is
			// refused by the client itself, with nothing sent: that is the
			// client declining to guess, and says nothing of the kind asserted
			// here.
			if isStaleDefinition(err) {
				return nil
			}
			return err
		}},
	}

	stop := make(chan struct{})
	var pinning sync.WaitGroup
	pinning.Add(1)
	go func() {
		defer pinning.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Either way the connection settles on the one revision the
			// endpoint speaks; what matters is that it is discarded.
			client.WithProtocolVersion(LatestProtocolVersion)
			client.WithProtocolVersion("")
		}
	}()

	failures := make(chan string, 64)
	var calling sync.WaitGroup
	for range 4 {
		calling.Add(1)
		go func() {
			defer calling.Done()
			// The version costs the wire nothing while a connection stands, so
			// it is asked for the most: the more often the question is put, the
			// more often a pin is waiting right behind it.
			for round := range 960 {
				for index, tc := range calls {
					if index > 0 && round%16 != 0 {
						continue
					}
					if err := tc.call(); err != nil {
						select {
						case failures <- tc.name + ": " + err.Error():
						default:
						}
					}
				}
			}
		}()
	}
	calling.Wait()
	close(stop)
	pinning.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("%s", failure)
	}
}

func TestNegotiatesOnDemandRatherThanGuessing(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)

	if c.Connected() {
		t.Fatal("a fresh client must not report a connection")
	}
	if _, err := c.ProtocolVersion(context.Background()); err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if !c.Connected() {
		t.Fatal("asking for the protocol version must negotiate one")
	}
}

func TestServerDetailsAreReportedForEitherHandshake(t *testing.T) {
	tests := []struct {
		name   string
		script []string
	}{
		{name: "discovery", script: []string{discoverFrame(LatestProtocolVersion)}},
		{name: "initialize", script: []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newScriptedClient(newScriptedTransport(tc.script...))

			info, err := c.ServerInfo(context.Background())
			if err != nil {
				t.Fatalf("server info: %v", err)
			}
			if info.Name != "Test Server" || info.Version != "1.0.0" {
				t.Fatalf("server info = %+v", info)
			}
			instructions, err := c.Instructions(context.Background())
			if err != nil || instructions != "Be nice." {
				t.Fatalf("instructions = %q err=%v", instructions, err)
			}
			capabilities, err := c.Capabilities(context.Background())
			if err != nil || capabilities == nil {
				t.Fatalf("capabilities = %v err=%v", capabilities, err)
			}
		})
	}
}

func TestDiscoverResultIgnoresUnusableVersionEntries(t *testing.T) {
	s := newScriptedTransport(resultFrame(map[string]any{
		"supportedVersions": []any{42, LatestProtocolVersion, nil},
		"capabilities":      map[string]any{},
	}))
	c := newScriptedClient(s)

	version, err := c.ProtocolVersion(context.Background())
	if err != nil {
		t.Fatalf("protocol version: %v", err)
	}
	if version != LatestProtocolVersion {
		t.Fatalf("negotiated version = %q, want %q", version, LatestProtocolVersion)
	}
	if got := c.DiscoverResult().SupportedVersions; !slices.Equal(got, []string{LatestProtocolVersion}) {
		t.Fatalf("supported versions = %v", got)
	}
	if info := c.DiscoverResult().ServerInfo; info.Name != "" {
		t.Fatalf("server info = %+v, want none", info)
	}
}

// TestAServerErrorLeavesTheConnectionUp pins the difference between a server
// answering on protocol terms and the channel failing: an ordinary JSON-RPC
// error (an unknown tool, say) is an outcome of the exchange, so the connection
// stands and the next request goes straight out without a second handshake.
func TestAServerErrorLeavesTheConnectionUp(t *testing.T) {
	tests := []struct {
		name string
		// handshake is the script that settles the connection.
		handshake []string
		// prelude answers the catalogue read a call by name makes on a
		// revision that mirrors arguments into request headers.
		prelude []string
		// wantMethods is every method the two calls put on the wire.
		wantMethods []string
	}{
		{
			name:        "a discovery era connection",
			handshake:   []string{discoverFrame(LatestProtocolVersion)},
			prelude:     []string{emptyToolsFrame()},
			wantMethods: []string{"tools/list", "tools/call", "tools/call"},
		},
		{
			name:        "an initialize era connection",
			handshake:   []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)},
			wantMethods: []string{"tools/call", "tools/call"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(tc.handshake...)
			c := newScriptedClient(s)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			connects, disconnects, _ := s.lifecycle()

			s.script(append(tc.prelude,
				errorFrame(jsonrpc.CodeInvalidParams, "Tool [nope] does not exist.", nil),
				resultFrame(map[string]any{"content": []any{}, "isError": false}),
			)...)

			_, err := c.CallTool(context.Background(), "nope", nil)

			var rpcErr *jsonrpc.Error
			if !errors.As(err, &rpcErr) {
				t.Fatalf("error = %v, want a JSON-RPC error", err)
			}
			if rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("error code = %d, want %d", rpcErr.Code, jsonrpc.CodeInvalidParams)
			}
			if !c.Connected() {
				t.Fatal("a JSON-RPC error from the server must not drop the connection")
			}
			gotConnects, gotDisconnects, connected := s.lifecycle()
			if gotConnects != connects {
				t.Fatalf("transport connects = %d, want %d: the connection was renegotiated", gotConnects, connects)
			}
			if gotDisconnects != disconnects {
				t.Fatalf("transport disconnects = %d, want %d: the channel was torn down", gotDisconnects, disconnects)
			}
			if !connected {
				t.Fatal("the transport was torn down by a server error")
			}

			if _, err := c.CallTool(context.Background(), "add", nil); err != nil {
				t.Fatalf("the call after a server error: %v", err)
			}
			if got := s.methods(); !slices.Equal(got, tc.wantMethods) {
				t.Fatalf("methods sent after the server error = %v, want %v and no handshake", got, tc.wantMethods)
			}
		})
	}
}

// TestAChannelFailureTakesTheConnectionDown is the other half: a failure of the
// channel itself leaves the stream out of step with the server, so the
// connection goes down and the next request negotiates a fresh one.
func TestAChannelFailureTakesTheConnectionDown(t *testing.T) {
	tests := []struct {
		name string
		// breakChannel puts the transport into the state that fails the next
		// request.
		breakChannel func(*scriptedTransport)
		wantErr      string
	}{
		{
			name: "the transport will not carry the frame",
			breakChannel: func(s *scriptedTransport) {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.sendErr = NewTransportError("the endpoint went away", nil)
			},
			wantErr: "the endpoint went away",
		},
		{
			name: "the server answers with a frame that is not JSON-RPC 2.0",
			breakChannel: func(s *scriptedTransport) {
				s.script(`{"jsonrpc":"1.0","id":1,"result":{}}`)
			},
			wantErr: "invalid JSON-RPC response from server",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(discoverFrame(LatestProtocolVersion))
			c := newScriptedClient(s)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			_, disconnects, _ := s.lifecycle()

			tc.breakChannel(s)

			_, err := c.CallTool(context.Background(), "add", nil)
			if err == nil {
				t.Fatalf("expected the call to fail with %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
			if c.Connected() {
				t.Fatal("a broken channel must drop the connection")
			}
			if _, gotDisconnects, _ := s.lifecycle(); gotDisconnects != disconnects+1 {
				t.Fatalf("transport disconnects = %d, want %d: the channel was left up", gotDisconnects, disconnects+1)
			}

			s.script(discoverFrame(LatestProtocolVersion), resultFrame(map[string]any{"tools": []any{}}))

			if _, err := c.Tools(context.Background()); err != nil {
				t.Fatalf("the call after the failure: %v", err)
			}
			if got := s.methods(); !slices.Equal(got, []string{"server/discover", "tools/list"}) {
				t.Fatalf("methods sent after the failure = %v, want a fresh handshake then the request", got)
			}
		})
	}
}

// TestAFailedProbeKeepsTheChannelForTheFallback pins that the probe does not
// tear the transport down when discovery is rejected: the fallback handshake
// runs over the same channel, and only the negotiation decides when to give up.
func TestAFailedProbeKeepsTheChannelForTheFallback(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	_, disconnects, connected := s.lifecycle()
	if disconnects != 0 {
		t.Fatalf("the probe tore the transport down %d time(s) before the fallback settled", disconnects)
	}
	if !connected {
		t.Fatal("the transport is down after a handshake that succeeded")
	}
}

// TestTheFallbackRestartsAChannelThatWasAlreadyGone pins the recovery a server
// that predates discovery needs when it answers the probe on protocol terms and
// then ends. Its refusal is not a failure of the channel, so nothing tears the
// channel down and the transport reports itself connected; the fallback only
// learns the peer is gone when it writes the initialize frame. Connecting again
// at that point is what starts the server, so the handshake settles rather than
// being reported against a channel that had already died.
func TestTheFallbackRestartsAChannelThatWasAlreadyGone(t *testing.T) {
	s := newScriptedTransport(
		methodNotFoundFrame(),
		initializeFrame(ProtocolV20251125),
		resultFrame(map[string]any{"tools": []any{map[string]any{"name": "legacy-tool"}}}),
	)
	s.failOnce["initialize"] = NewTransportError(
		"subprocess [server] closed its output before sending a complete response", nil)

	c := newScriptedClient(s)
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "legacy-tool" {
		t.Fatalf("tools = %v, want the one the restarted server lists", tools)
	}

	wantMethods := []string{"server/discover", "initialize", "notifications/initialized", "tools/list"}
	if got := s.methods(); !slices.Equal(got, wantMethods) {
		t.Fatalf("sent methods = %v, want %v", got, wantMethods)
	}
	version, err := c.ProtocolVersion(context.Background())
	if err != nil || version != ProtocolV20251125 {
		t.Fatalf("negotiated version = %q (%v), want %q", version, err, ProtocolV20251125)
	}

	// One connect for the probe, one for the fallback, and one for the restart,
	// with the dead channel torn down in between rather than adopted.
	connects, disconnects, connected := s.lifecycle()
	if connects != 3 {
		t.Fatalf("the transport was connected %d time(s), want 3", connects)
	}
	if disconnects != 1 {
		t.Fatalf("the transport was disconnected %d time(s), want 1", disconnects)
	}
	if !connected {
		t.Fatal("the transport is down after a handshake that settled")
	}
}

// TestTheFallbackRetriesOnlyAChannelFailure pins what the restart is for. A
// channel that failed is worth a second channel; an answer on protocol terms, a
// server that owes a reply it never sent, and a missing credential are not, and
// spending a reconnect on any of them would only fail the same way twice.
func TestTheFallbackRetriesOnlyAChannelFailure(t *testing.T) {
	channelFailure := NewTransportError("the subprocess went away", nil)
	authErr := &oauth.AuthorizationRequiredError{Message: "authorization is required"}

	tests := []struct {
		name string
		// arrange prepares the transport after the probe rejection is scripted.
		arrange func(*scriptedTransport)
		// wantConnects counts the probe's connect, the fallback's, and the
		// restart's when there is one.
		wantConnects int
		wantErr      string
	}{
		{
			name:         "a channel failure that persists",
			arrange:      func(s *scriptedTransport) { s.failOn["initialize"] = channelFailure },
			wantConnects: 3,
			wantErr:      "jsonrpc: code -32601: Method not found.; the legacy handshake also failed: the subprocess went away",
		},
		{
			name: "a server that owes a reply",
			arrange: func(s *scriptedTransport) {
				s.failOn["initialize"] = NewTimeoutError("timed out while waiting for server response", nil)
			},
			wantConnects: 2,
			wantErr: "jsonrpc: code -32601: Method not found.; the legacy handshake also failed: " +
				"timed out while waiting for server response",
		},
		{
			name:         "a refusal on protocol terms",
			arrange:      func(s *scriptedTransport) { s.script(errorFrame(jsonrpc.CodeInternalError, "Internal error", nil)) },
			wantConnects: 2,
			wantErr:      "jsonrpc: code -32601: Method not found.; the legacy handshake also failed: jsonrpc: code -32603: Internal error",
		},
		{
			name:         "a missing credential",
			arrange:      func(s *scriptedTransport) { s.failOn["initialize"] = authErr },
			wantConnects: 2,
			wantErr:      "authorization is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScriptedTransport(methodNotFoundFrame())
			tt.arrange(s)

			err := newScriptedClient(s).Connect(context.Background())
			if err == nil {
				t.Fatal("expected the handshake to fail")
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
			}
			if connects, _, _ := s.lifecycle(); connects != tt.wantConnects {
				t.Fatalf("the transport was connected %d time(s), want %d", connects, tt.wantConnects)
			}
		})
	}
}

// TestAnAbandonedProbeDoesNotAttemptTheFallback pins that a caller withdrawing
// the request ends the negotiation: there is no reconnect and no second
// handshake over a context that is already done.
func TestAnAbandonedProbeDoesNotAttemptTheFallback(t *testing.T) {
	s := newScriptedTransport(initializeFrame(ProtocolV20251125))
	s.failOn["server/discover"] = NewTransportError("the wait was cancelled", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.beforeSend = func(method string) {
		if method == "server/discover" {
			cancel()
		}
	}

	err := newScriptedClient(s).Connect(ctx)

	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if err.Error() != "the wait was cancelled" {
		t.Fatalf("error = %q, want the probe rejection alone", err.Error())
	}
	if got := s.methods(); len(got) != 0 {
		t.Fatalf("frames sent over a cancelled context = %v, want none", got)
	}
	if _, _, connected := s.lifecycle(); connected {
		t.Fatal("the transport was left connected after an abandoned handshake")
	}
}

func TestConcurrentRequestsShareOneHandshake(t *testing.T) {
	f := newFakeTransport()
	f.on("server/discover", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"supportedVersions": []any{LatestProtocolVersion},
			"capabilities":      map[string]any{},
		})
		return resp
	})
	c := New(f, testClientInfo())

	const callers = 8
	errs := make(chan error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- c.Ping(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ping: %v", err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connects != 1 {
		t.Fatalf("the transport was connected %d times, want one handshake", f.connects)
	}
	if len(f.sent) != callers+1 {
		t.Fatalf("sent %d frames, want %d (one handshake plus one per caller)", len(f.sent), callers+1)
	}
}
