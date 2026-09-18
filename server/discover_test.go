package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/server"
)

// TestDiscoverDocument asserts server/discover answers with everything a client
// needs before its first real call: which revisions it may speak, what the
// server can do, and how it wants to be used.
func TestDiscoverDocument(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithInstructions("Be nice."), server.WithTools(addTool()))

	result := decodeResult(t, handle(t, s, modernRequest(456, "server/discover")).Response)

	versions, ok := result["supportedVersions"].([]any)
	if !ok || len(versions) != 1 || versions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions = %#v", result["supportedVersions"])
	}
	if result["instructions"] != "Be nice." {
		t.Fatalf("instructions = %v", result["instructions"])
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %#v", result["capabilities"])
	}
	for _, want := range []string{"tools", "resources", "prompts"} {
		if _, present := caps[want]; !present {
			t.Fatalf("capability %q missing from %v", want, caps)
		}
	}
	// The server identity travels in the result envelope, not in the body: a
	// duplicate serverInfo member would be two sources of the same truth.
	if _, duplicated := result["serverInfo"]; duplicated {
		t.Fatalf("discover result should not repeat serverInfo: %v", result)
	}
	if info := serverInfoOf(t, result); info["name"] != "demo" {
		t.Fatalf("server info = %v", info)
	}
}

// TestDiscoverEmptyCapabilityIsAnObject asserts a capability that carries no
// configuration is encoded as {} and never as null, which a client would read
// as "this capability is absent" rather than "present with no options".
func TestDiscoverEmptyCapabilityIsAnObject(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithCapability("anotherFeature"))
	res := handle(t, s, modernRequest(1, "server/discover"))

	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(res.Response.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got := string(result.Capabilities["anotherFeature"]); got != "{}" {
		t.Fatalf("anotherFeature = %s, want {}", got)
	}
}

// TestDiscoverReportsConfiguredVersions asserts the advertised list is the
// server's own, so a deployment pinned to another revision publishes it.
func TestDiscoverReportsConfiguredVersions(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithProtocolVersions("2026-07-28", "2027-01-01"))
	result := decodeResult(t, handle(t, s, modernRequest(1, "server/discover")).Response)
	versions := result["supportedVersions"].([]any)
	if len(versions) != 2 || versions[0] != "2026-07-28" || versions[1] != "2027-01-01" {
		t.Fatalf("supportedVersions = %v", versions)
	}
}

// TestDiscoverRequiresProtocolMetadata asserts discovery states the revision it
// is made under like every other request of its handshake. The method arrived
// with that handshake and has no earlier form, so the exemption that carries
// clients predating the metadata cannot carry a request for it; a client that
// names a revision the server does not speak learns the supported set from the
// dedicated refusal rather than from a document served on unstated terms.
func TestDiscoverRequiresProtocolMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no params", `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`},
		{"empty params", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`},
		{"metadata is an empty list", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":[]}}`},
		{"metadata is a string", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":"nope"}}`},
		{"unrelated metadata only", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"progressToken":7}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, tt.raw)

			encoded, err := json.Marshal(res.Response)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			const want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params: The request [_meta] is missing the required [io.modelcontextprotocol/protocolVersion] member."}}`
			if string(encoded) != want {
				t.Fatalf("wire form =\n%s\nwant\n%s", encoded, want)
			}
		})
	}
}

// TestDiscoverRenegotiatesAnUnsupportedRevision asserts the opening request is
// answered on protocol terms when it names a revision the server does not speak:
// the client is told which revisions it may retry with, which is how it learns
// the supported set without ever reading the discovery document.
func TestDiscoverRenegotiatesAnUnsupportedRevision(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}}`)

	encoded, err := json.Marshal(res.Response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	const want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":"2025-11-25","supported":["2026-07-28"]}}}`
	if string(encoded) != want {
		t.Fatalf("wire form =\n%s\nwant\n%s", encoded, want)
	}
}

// TestDiscoverAssignsNoSession asserts discovery establishes nothing: unlike
// initialize, it is a plain read of the server's capabilities.
func TestDiscoverAssignsNoSession(t *testing.T) {
	s := server.New("demo", "1.0.0")
	s.SetSessionIDGenerator(func() string { return "sess-1" })
	if id := handle(t, s, modernRequest(1, "server/discover")).SessionID; id != "" {
		t.Fatalf("discover assigned session %q", id)
	}
}

// collectFrames drives a message through the streaming entry point and returns
// the frames emitted before the reply, plus the reply itself.
func collectFrames(t *testing.T, s *server.Server, raw string) ([]map[string]any, map[string]any) {
	t.Helper()
	var frames []map[string]any
	emit := func(msg []byte) error {
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			return err
		}
		frames = append(frames, frame)
		return nil
	}
	res := s.HandleStream(context.Background(), []byte(raw), "", emit)
	return frames, decodeResult(t, res.Response)
}

// TestSubscriptionsListen asserts the subscription lifecycle a client sees: an
// acknowledgement naming the notifications it will actually receive (none),
// then an immediate close, both tagged with the subscription id so the client
// can retire it.
func TestSubscriptionsListen(t *testing.T) {
	s := server.New("demo", "1.0.0")

	frames, result := collectFrames(t, s,
		modernRequest(7, "subscriptions/listen", `"notifications":{"toolsListChanged":true}`))

	if len(frames) != 1 {
		t.Fatalf("expected exactly one notification, got %d", len(frames))
	}
	ack := frames[0]
	if ack["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("notification method = %v", ack["method"])
	}
	params := ack["params"].(map[string]any)
	subscribed, ok := params["notifications"].(map[string]any)
	if !ok || len(subscribed) != 0 {
		t.Fatalf("acknowledged notifications = %#v, want an empty object", params["notifications"])
	}
	ackMeta := params["_meta"].(map[string]any)
	if ackMeta[server.MetaKeySubscriptionID] != float64(7) {
		t.Fatalf("acknowledgement subscription id = %v", ackMeta[server.MetaKeySubscriptionID])
	}

	resultMeta := result["_meta"].(map[string]any)
	if resultMeta[server.MetaKeySubscriptionID] != float64(7) {
		t.Fatalf("result subscription id = %v", resultMeta[server.MetaKeySubscriptionID])
	}
	if result["resultType"] != "complete" {
		t.Fatalf("resultType = %v", result["resultType"])
	}
}

// TestSubscriptionsListenEchoesTheIDForm asserts the subscription id keeps the
// JSON type the client used for the request id, since that is what the client
// matches on.
func TestSubscriptionsListenEchoesTheIDForm(t *testing.T) {
	s := server.New("demo", "1.0.0")
	raw := `{"jsonrpc":"2.0","id":"sub-a","method":"subscriptions/listen","params":{` + protocolMeta + `}}`

	var frames [][]byte
	emit := func(msg []byte) error {
		frames = append(frames, append([]byte(nil), msg...))
		return nil
	}
	res := s.HandleStream(context.Background(), []byte(raw), "", emit)

	if len(frames) != 1 {
		t.Fatalf("expected one notification, got %d", len(frames))
	}
	const want = `"io.modelcontextprotocol/subscriptionId":"sub-a"`
	for _, encoded := range append(frames, res.Response.Result) {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("frame does not carry the string subscription id: %s", encoded)
		}
	}
}

// TestSubscriptionsListenWithoutAStream asserts the request still completes on
// a transport that cannot push frames: the client learns the outcome from the
// result rather than waiting on an acknowledgement it will never get.
func TestSubscriptionsListenWithoutAStream(t *testing.T) {
	s := server.New("demo", "1.0.0")
	result := decodeResult(t, handle(t, s, modernRequest(7, "subscriptions/listen")).Response)
	meta := result["_meta"].(map[string]any)
	if meta[server.MetaKeySubscriptionID] != float64(7) {
		t.Fatalf("subscription id = %v", meta[server.MetaKeySubscriptionID])
	}
}
