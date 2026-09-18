package methods

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// TestDiscoverHandler asserts the handler reports the server's own advertised
// versions, capabilities and instructions rather than package defaults.
func TestDiscoverHandler(t *testing.T) {
	c := ctxWith(
		server.WithInstructions("Be nice."),
		server.WithProtocolVersions("2026-07-28"),
		server.WithTools(echoTool()),
	)
	resp, err := Discover{}.Handle(c, req(t, 1, "server/discover", nil))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	m := decodeResult(t, resp)

	versions := m["supportedVersions"].([]any)
	if len(versions) != 1 || versions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions = %v", versions)
	}
	if m["instructions"] != "Be nice." {
		t.Fatalf("instructions = %v", m["instructions"])
	}
	if _, ok := m["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities = %v", m["capabilities"])
	}
}

// protocolMeta is the params._meta bag every discovery-handshake request
// carries. subscriptions/listen arrived with that handshake, so a request for it
// is only well formed with the bag in place.
const protocolMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// listenRequest builds a well-formed subscriptions/listen request with the given
// id and extra params (which may be empty).
func listenRequest(id string, params string) string {
	if params != "" {
		params = "," + params
	}
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"subscriptions/listen","params":{` + protocolMeta + params + `}}`
}

// listenFrames drives a subscriptions/listen request through a server with the
// given streaming sink, returning the emitted frames and the final result.
func listenFrames(t *testing.T, id string, params string, emit func([]byte) error) ([]map[string]any, map[string]any) {
	t.Helper()
	s := server.New("test", "1.0.0")
	raw := listenRequest(id, params)

	var frames []map[string]any
	sink := func(msg []byte) error {
		if emit != nil {
			if err := emit(msg); err != nil {
				return err
			}
		}
		var frame map[string]any
		if err := json.Unmarshal(msg, &frame); err != nil {
			return err
		}
		frames = append(frames, frame)
		return nil
	}

	res := s.HandleStream(context.Background(), []byte(raw), "", sink)
	if !res.HasResponse || res.Response == nil {
		t.Fatal("expected a response")
	}
	if res.Response.Error != nil {
		return frames, nil
	}
	var result map[string]any
	if err := json.Unmarshal(res.Response.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return frames, result
}

// TestListenAcknowledgesThenCloses asserts the subscription lifecycle: one
// acknowledgement frame naming no notifications (this server pushes none),
// then a result, both tagged with the subscription id.
func TestListenAcknowledgesThenCloses(t *testing.T) {
	frames, result := listenFrames(t, "7", `"notifications":{"toolsListChanged":true}`, nil)

	if len(frames) != 1 {
		t.Fatalf("expected one emitted frame, got %d", len(frames))
	}
	ack := frames[0]
	if ack["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("method = %v", ack["method"])
	}
	if _, isResult := ack["result"]; isResult {
		t.Fatalf("the acknowledgement must be a notification, not a result: %v", ack)
	}
	params := ack["params"].(map[string]any)
	if got, ok := params["notifications"].(map[string]any); !ok || len(got) != 0 {
		t.Fatalf("notifications = %#v, want an empty object", params["notifications"])
	}
	if params["_meta"].(map[string]any)[server.MetaKeySubscriptionID] != float64(7) {
		t.Fatalf("acknowledgement _meta = %v", params["_meta"])
	}
	if result["_meta"].(map[string]any)[server.MetaKeySubscriptionID] != float64(7) {
		t.Fatalf("result _meta = %v", result["_meta"])
	}
}

// TestListenEchoesTheIDForm asserts a string request id comes back as a string
// subscription id, since that is what the client matches on.
func TestListenEchoesTheIDForm(t *testing.T) {
	frames, result := listenFrames(t, `"sub-a"`, "", nil)
	if len(frames) != 1 {
		t.Fatalf("expected one emitted frame, got %d", len(frames))
	}
	if got := frames[0]["params"].(map[string]any)["_meta"].(map[string]any)[server.MetaKeySubscriptionID]; got != "sub-a" {
		t.Fatalf("acknowledgement subscription id = %#v", got)
	}
	if got := result["_meta"].(map[string]any)[server.MetaKeySubscriptionID]; got != "sub-a" {
		t.Fatalf("result subscription id = %#v", got)
	}
}

// errStreamClosed stands in for a transport whose peer has gone away.
var errStreamClosed = errors.New("stream closed")

// TestListenSurfacesEmitFailures asserts a failed write is not swallowed: a
// client that never received the acknowledgement must not be told the
// subscription opened cleanly. The failure reaches the client as a generic
// internal error, with the underlying cause kept server-side.
func TestListenSurfacesEmitFailures(t *testing.T) {
	s := server.New("test", "1.0.0")
	sink := func([]byte) error { return errStreamClosed }
	res := s.HandleStream(context.Background(), []byte(listenRequest("1", "")), "", sink)

	if res.Response == nil || res.Response.Error == nil {
		t.Fatalf("expected an error response, got %+v", res.Response)
	}
	if res.Response.Error.Code != jsonrpc.CodeInternalError {
		t.Fatalf("code = %d, want %d", res.Response.Error.Code, jsonrpc.CodeInternalError)
	}
	// The wire form is what the client reads: a generic internal error, keyed to
	// the request it answers, with no trace of the cause kept server-side.
	encoded, err := json.Marshal(res.Response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	const want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Something went wrong while processing the request."}}`
	if string(encoded) != want {
		t.Fatalf("wire form =\n%s\nwant\n%s", encoded, want)
	}
	if strings.Contains(string(encoded), errStreamClosed.Error()) {
		t.Fatalf("the response leaks the underlying cause: %s", encoded)
	}
}

// TestListenWithoutAStream asserts the request still completes on a transport
// that cannot push frames at all: the client learns the outcome from the result.
func TestListenWithoutAStream(t *testing.T) {
	s := server.New("test", "1.0.0")
	res := s.Handle(context.Background(), []byte(listenRequest("3", "")), "")
	result := decodeResult(t, res.Response)
	if result["_meta"].(map[string]any)[server.MetaKeySubscriptionID] != float64(3) {
		t.Fatalf("result _meta = %v", result["_meta"])
	}
}

// TestListenRequiresProtocolMetadata asserts a subscriptions/listen request that
// states no protocol metadata is refused rather than served. The method arrived
// with the discovery handshake, so no client predating that metadata can be
// asking for it, and the exemption that carries older clients must not carry a
// malformed request instead.
func TestListenRequiresProtocolMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no params", `{"jsonrpc":"2.0","id":4,"method":"subscriptions/listen"}`},
		{"empty params", `{"jsonrpc":"2.0","id":4,"method":"subscriptions/listen","params":{}}`},
		{"metadata is an empty list", `{"jsonrpc":"2.0","id":4,"method":"subscriptions/listen","params":{"_meta":[]}}`},
		{"unrelated metadata only", `{"jsonrpc":"2.0","id":4,"method":"subscriptions/listen","params":{"_meta":{"progressToken":7}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("test", "1.0.0")
			var frames int
			sink := func([]byte) error { frames++; return nil }
			res := s.HandleStream(context.Background(), []byte(tt.raw), "", sink)

			if frames != 0 {
				t.Fatalf("a refused subscription emitted %d frames", frames)
			}
			encoded, err := json.Marshal(res.Response)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			const want = `{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"Invalid params: The request [_meta] is missing the required [io.modelcontextprotocol/protocolVersion] member."}}`
			if string(encoded) != want {
				t.Fatalf("wire form =\n%s\nwant\n%s", encoded, want)
			}
		})
	}
}
