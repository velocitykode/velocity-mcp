package client

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// This file covers the result envelope as a whole: every result the protocol
// defines is an object, and one stating a result type the specification does
// not define is invalid. The checks hold for every request, the handshake
// included, since a handshake result that passed them by would settle a
// connection and be kept for its lifetime on the strength of a result the
// specification says to refuse.

// rawResultFrame builds a response whose result member is written exactly as
// given, so a shape no helper would build can be put on the wire.
func rawResultFrame(result string) string {
	return `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":` + result + `}`
}

// TestADiscoverResultIsValidatedLikeAnyOther asserts the handshake result goes
// through the same validation as every other result: a result type the
// specification does not define, one that is not a string, an input_required
// result (which the specification forbids on server/discover), and a result
// that is not an object are each refused with the reason, the connection is
// not settled, nothing is kept of the result, and the next connect probes again.
func TestADiscoverResultIsValidatedLikeAnyOther(t *testing.T) {
	discover := `"supportedVersions":["` + LatestProtocolVersion + `"],"capabilities":{},"ttlMs":600000`
	tests := []struct {
		name    string
		result  string
		wantErr string
	}{
		{
			name:    "an unrecognized result type",
			result:  `{"resultType":"bogus",` + discover + `}`,
			wantErr: "the server answered [server/discover] with an invalid result: it states the unrecognized result type [bogus]",
		},
		{
			name:    "a result type that is not a string",
			result:  `{"resultType":17,` + discover + `}`,
			wantErr: "the server answered [server/discover] with an invalid result: its result type is not a string",
		},
		{
			name:    "an unfinished result",
			result:  `{"resultType":"input_required","requestState":"opaque",` + discover + `}`,
			wantErr: "the server answered [server/discover] with an invalid result: it asks for further input, which a handshake never does",
		},
		{
			name:    "a result that is not an object",
			result:  `null`,
			wantErr: "the server answered [server/discover] with an invalid result: it is not an object",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(rawResultFrame(tc.result), discoverFrame(LatestProtocolVersion))
			c := newScriptedClient(s)

			err := c.Connect(context.Background())
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("connect = %v, want %q", err, tc.wantErr)
			}
			if c.Connected() {
				t.Fatal("a refused discover result settled the connection")
			}
			if c.DiscoverResult() != nil {
				t.Fatal("a refused discover result was kept")
			}
			if _, err := c.Capabilities(context.Background()); err != nil {
				t.Fatalf("capabilities over a valid result: %v", err)
			}
			if want := []string{"server/discover", "server/discover"}; !slices.Equal(s.methods(), want) {
				t.Fatalf("methods = %v, want %v: the refused handshake was not probed again", s.methods(), want)
			}
		})
	}
}

// TestAnInitializeResultIsValidatedLikeAnyOther asserts the initialize handshake
// is held to the same envelope: a result type the specification does not
// define, an unfinished result, and a result that is not an object are refused
// and settle nothing.
func TestAnInitializeResultIsValidatedLikeAnyOther(t *testing.T) {
	settled := `"protocolVersion":"` + ProtocolV20251125 + `","capabilities":{},"serverInfo":{"name":"s","version":"1"}`
	tests := []struct {
		name    string
		result  string
		wantErr string
	}{
		{
			name:    "an unrecognized result type",
			result:  `{"resultType":"bogus",` + settled + `}`,
			wantErr: "the server answered [initialize] with an invalid result: it states the unrecognized result type [bogus]",
		},
		{
			name:    "an unfinished result",
			result:  `{"resultType":"input_required","requestState":"opaque",` + settled + `}`,
			wantErr: "the server answered [initialize] with an invalid result: it asks for further input, which a handshake never does",
		},
		{
			name:    "a result that is not an object",
			result:  `[]`,
			wantErr: "the server answered [initialize] with an invalid result: it is not an object",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(methodNotFoundFrame(), rawResultFrame(tc.result))
			c := newScriptedClient(s)

			err := c.Connect(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("connect = %v, want it to carry %q", err, tc.wantErr)
			}
			if c.Connected() {
				t.Fatal("a refused initialize result settled the connection")
			}
			if c.InitializeResult() != nil {
				t.Fatal("a refused initialize result was kept")
			}
			if want := []string{"server/discover", "initialize"}; !slices.Equal(s.methods(), want) {
				t.Fatalf("methods = %v, want %v: the initialized notification followed a refused result", s.methods(), want)
			}
		})
	}
}

// TestAResultThatIsNotAnObjectIsInvalid asserts a result of any other JSON type
// is refused on every request rather than decoded as an empty success: a null
// tools/call result would otherwise report a call that returned nothing, and a
// null answer to a liveness check would report a server that answered nothing
// as alive. The refusal is the server's answer, so the connection stands.
func TestAResultThatIsNotAnObjectIsInvalid(t *testing.T) {
	requests := []struct {
		name string
		// method is the request named in the error, and prelude what the call
		// reads before it.
		method  string
		prelude []string
		call    func(*Client) error
	}{
		{
			name:    "tools/call",
			method:  "tools/call",
			prelude: []string{emptyToolsFrame()},
			call: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "execute_sql", nil)
				return err
			},
		},
		{
			name:   "resources/read",
			method: "resources/read",
			call: func(c *Client) error {
				_, err := c.ReadResource(context.Background(), "file:///notes.txt")
				return err
			},
		},
		{
			name:   "prompts/get",
			method: "prompts/get",
			call: func(c *Client) error {
				_, err := c.GetPrompt(context.Background(), "review", nil)
				return err
			},
		},
		{
			name:   "tools/list",
			method: "tools/list",
			call: func(c *Client) error {
				_, err := c.Tools(context.Background())
				return err
			},
		},
		{
			// On the discovery revision the liveness check is server/discover.
			name:   "liveness",
			method: "server/discover",
			call:   func(c *Client) error { return c.Ping(context.Background()) },
		},
	}
	shapes := []string{`null`, `[]`, `"done"`, `7`, `true`}

	for _, request := range requests {
		for _, shape := range shapes {
			t.Run(request.name+"/"+shape, func(t *testing.T) {
				c, s := discoveryClient(t, append(request.prelude, rawResultFrame(shape))...)

				err := request.call(c)
				want := "the server answered [" + request.method + "] with an invalid result: it is not an object"
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %q", err, want)
				}
				if !c.Connected() {
					t.Fatal("an invalid result took the connection down")
				}
				if _, disconnects, _ := s.lifecycle(); disconnects != 0 {
					t.Fatalf("the transport was torn down %d time(s) over an invalid result", disconnects)
				}
			})
		}
	}
}

// TestALegacyPingResultThatIsNotAnObjectIsInvalid asserts the same for the ping
// request of the initialize-era revisions, whose result is an empty object and
// nothing else.
func TestALegacyPingResultThatIsNotAnObjectIsInvalid(t *testing.T) {
	for _, shape := range []string{`null`, `[]`, `0`} {
		t.Run(shape, func(t *testing.T) {
			s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125), rawResultFrame(shape))
			c := newScriptedClient(s)

			err := c.Ping(context.Background())
			want := "the server answered [ping] with an invalid result: it is not an object"
			if err == nil || err.Error() != want {
				t.Fatalf("ping = %v, want %q", err, want)
			}
			if !c.Connected() {
				t.Fatal("an invalid result took the connection down")
			}
		})
	}
}
