package client

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers a server that stops accepting the protocol version a
// connection was settled on. The MCP specification (2026-07-28,
// basic/lifecycle#version-negotiation) has a client that is answered with
// UnsupportedProtocolVersion select a version both peers support and try again,
// and probe again when what it assumed of the server no longer holds: a server
// may be rolled back, and a request may reach another replica than the one that
// answered the handshake.

// unsupportedFrame is the rejection of a request made at a version the server
// does not accept, naming the versions it does.
func unsupportedFrame(supported ...string) string {
	return errorFrame(CodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
		"supported": supported,
		"requested": LatestProtocolVersion,
	})
}

// promptsFrame is a prompts/list result carrying one prompt.
func promptsFrame() string {
	return resultFrame(map[string]any{"prompts": []any{map[string]any{"name": "p"}}})
}

// TestAVersionRefusedAfterTheHandshakeIsRenegotiated asserts what the client
// does when a request over a settled connection is refused for its version:
// the handshake runs again, the request is repeated once over the connection it
// settles, and the rejection is the answer only where nothing else can be
// settled.
func TestAVersionRefusedAfterTheHandshakeIsRenegotiated(t *testing.T) {
	listPrompts := func(c *Client) error {
		prompts, err := c.Prompts(context.Background())
		if err == nil && (len(prompts) != 1 || prompts[0].Name != "p") {
			return newError("the listing did not carry the server's prompt")
		}
		return err
	}
	renegotiatedDown := []string{
		"server/discover", "prompts/list",
		"prompts/list", "server/discover", "initialize", "notifications/initialized", "prompts/list",
	}

	tests := []struct {
		name string
		// pin is the version the client is pinned to, if any.
		pin ProtocolVersion
		// settle is the script of the connection before the server changes, and
		// first the request made over it.
		settle []string
		first  func(c *Client) error
		// after is the script from the refused request on, and request the
		// request that meets the refusal.
		after   []string
		request func(c *Client) error
		// wantRefused is whether the rejection is what the caller gets.
		wantRefused bool
		wantMethods []string
		wantVersion ProtocolVersion
	}{
		{
			name:        "the server refuses the probe too, naming an older revision",
			settle:      []string{discoverFrame(LatestProtocolVersion), promptsFrame()},
			first:       listPrompts,
			after:       []string{unsupportedFrame(ProtocolV20251125), unsupportedFrame(ProtocolV20251125), initializeFrame(ProtocolV20251125), promptsFrame()},
			request:     listPrompts,
			wantMethods: renegotiatedDown,
			wantVersion: ProtocolV20251125,
		},
		{
			name:        "the server answers the probe with the versions it now supports",
			settle:      []string{discoverFrame(LatestProtocolVersion), promptsFrame()},
			first:       listPrompts,
			after:       []string{unsupportedFrame(ProtocolV20251125), discoverFrame(ProtocolV20251125), initializeFrame(ProtocolV20251125), promptsFrame()},
			request:     listPrompts,
			wantMethods: renegotiatedDown,
			wantVersion: ProtocolV20251125,
		},
		{
			name:   "a tool call is repeated on the terms of the revision settled again",
			settle: []string{discoverFrame(LatestProtocolVersion), toolsFrame(annotatedTool("Region")), toolCallFrame("first")},
			first: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
				return err
			},
			after: []string{unsupportedFrame(ProtocolV20251125), unsupportedFrame(ProtocolV20251125), initializeFrame(ProtocolV20251125), toolCallFrame("done")},
			request: func(c *Client) error {
				result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
				if err == nil && result.Text() != "done" {
					return newError("text = " + result.Text())
				}
				return err
			},
			wantMethods: []string{
				"server/discover", "tools/list", "tools/call",
				"tools/call", "server/discover", "initialize", "notifications/initialized", "tools/call",
			},
			wantVersion: ProtocolV20251125,
		},
		{
			name:        "a server that refuses the repeat as well is reported, and asked no third time",
			settle:      []string{discoverFrame(LatestProtocolVersion), promptsFrame()},
			first:       listPrompts,
			after:       []string{unsupportedFrame(ProtocolV20251125), unsupportedFrame(ProtocolV20251125), initializeFrame(ProtocolV20251125), unsupportedFrame(ProtocolV20250618)},
			request:     listPrompts,
			wantRefused: true,
			wantMethods: renegotiatedDown,
			wantVersion: ProtocolV20251125,
		},
		{
			name:        "a handshake that settles the refused version again leaves the rejection standing",
			settle:      []string{discoverFrame(LatestProtocolVersion), promptsFrame()},
			first:       listPrompts,
			after:       []string{unsupportedFrame(LatestProtocolVersion), discoverFrame(LatestProtocolVersion)},
			request:     listPrompts,
			wantRefused: true,
			wantMethods: []string{"server/discover", "prompts/list", "prompts/list", "server/discover"},
			wantVersion: LatestProtocolVersion,
		},
		{
			name:        "a pinned version is not renegotiated",
			pin:         LatestProtocolVersion,
			settle:      []string{discoverFrame(LatestProtocolVersion), promptsFrame()},
			first:       listPrompts,
			after:       []string{unsupportedFrame(ProtocolV20251125)},
			request:     listPrompts,
			wantRefused: true,
			wantMethods: []string{"server/discover", "prompts/list", "prompts/list"},
			wantVersion: LatestProtocolVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScriptedTransport(append(append([]string{}, tt.settle...), tt.after...)...)
			c := newScriptedClient(s)
			if tt.pin != "" {
				c.WithProtocolVersion(tt.pin)
			}
			if err := tt.first(c); err != nil {
				t.Fatalf("the request before the server changed: %v", err)
			}

			err := tt.request(c)
			var rpcErr *jsonrpc.Error
			refused := errors.As(err, &rpcErr) && rpcErr.Code == CodeUnsupportedProtocolVersion
			if refused != tt.wantRefused || (err != nil && !refused) {
				t.Fatalf("error = %v, want the rejection = %v (methods = %v)", err, tt.wantRefused, s.methods())
			}
			if got := s.methods(); !slices.Equal(got, tt.wantMethods) {
				t.Fatalf("methods = %v, want %v", got, tt.wantMethods)
			}
			if !c.Connected() {
				t.Fatal("the refusal took the connection down")
			}
			// The version is that of the connection now standing, asked for
			// without another frame: the script has none left to answer one.
			version, err := c.ProtocolVersion(context.Background())
			if err != nil || version != tt.wantVersion {
				t.Fatalf("version = %q (%v), want %q", version, err, tt.wantVersion)
			}
		})
	}
}
