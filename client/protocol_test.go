package client

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// discoveryMeta is the protocol metadata every discovery-era request carries,
// as it is encoded on the wire for the identity the negotiation tests use.
const discoveryMeta = `"_meta":{"io.modelcontextprotocol/clientCapabilities":{},` +
	`"io.modelcontextprotocol/clientInfo":{"name":"Acme MCP App","version":"9.9.9"},` +
	`"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`

// discoveryMetaAsking is discoveryMeta of a request that asks for progress under
// the given token, which every request after the handshake does.
func discoveryMetaAsking(token string) string {
	return strings.TrimSuffix(discoveryMeta, "}") + `,"progressToken":` + token + `}`
}

// TestRequestParamsOnTheWire pins the exact params member a request carries in
// each protocol era, taken from the frame the client actually sent. A request
// the server may take long over carries a progress token of the client's own,
// numbered from one over the life of the client; every other request is sent
// as it was before there was one.
func TestRequestParamsOnTheWire(t *testing.T) {
	legacy := []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)}
	discovery := []string{discoverFrame(LatestProtocolVersion)}

	tests := []struct {
		name string
		// handshake is the script that settles the connection.
		handshake []string
		// reply answers the request under test.
		reply string
		// prelude answers whatever the call reads before the request itself.
		prelude []string
		call    func(*Client) error
		// wantParams is the exact params member, or empty when the request
		// carries none.
		wantParams string
	}{
		{
			name:      "a legacy request sends its params as given",
			handshake: legacy,
			reply:     resultFrame(map[string]any{"content": []any{}, "isError": false}),
			call: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "add", map[string]any{"a": 1})
				return err
			},
			wantParams: `{"_meta":{"progressToken":1},"arguments":{"a":1},"name":"add"}`,
		},
		{
			name:       "a legacy request without params sends none",
			handshake:  legacy,
			reply:      resultFrame(map[string]any{}),
			call:       func(c *Client) error { return c.Ping(context.Background()) },
			wantParams: "",
		},
		{
			name:       "a discovery request without params still carries the metadata",
			handshake:  discovery,
			reply:      resultFrame(map[string]any{}),
			call:       func(c *Client) error { return c.Ping(context.Background()) },
			wantParams: `{` + discoveryMeta + `}`,
		},
		{
			name:      "a discovery request keeps its own params alongside the metadata",
			handshake: discovery,
			prelude:   []string{emptyToolsFrame()},
			reply:     resultFrame(map[string]any{"content": []any{}, "isError": false}),
			call: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "add", map[string]any{"a": 1})
				return err
			},
			wantParams: `{` + discoveryMetaAsking("1") + `,"arguments":{"a":1},"name":"add"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(tc.handshake...)
			c := newScriptedClient(s)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			s.script(append(tc.prelude, tc.reply)...)

			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := s.rawMember(t, len(tc.prelude), "params"); got != tc.wantParams {
				t.Fatalf("params = %s, want %s", got, tc.wantParams)
			}
		})
	}
}

// TestOnlyALongRunningRequestAsksForProgress pins which requests carry a
// progress token, on each transport and each handshake. Calling a tool, reading
// a resource and getting a prompt may take a server long, and ask for progress
// so that its reports keep the timeout from cutting them. A liveness check, a
// listing, the catalogue a tool call reads first, and the handshake ask for
// none. Over the HTTP transport no request asks: the transport bounds a whole
// request by its own timeout, so progress could not keep one alive there.
func TestOnlyALongRunningRequestAsksForProgress(t *testing.T) {
	calls := []struct {
		method string
		call   func(c *Client) error
	}{
		{"ping", func(c *Client) error { return c.Ping(context.Background()) }},
		{"tools/list", func(c *Client) error { _, err := c.Tools(context.Background()); return err }},
		{"resources/list", func(c *Client) error { _, err := c.Resources(context.Background()); return err }},
		{"prompts/list", func(c *Client) error { _, err := c.Prompts(context.Background()); return err }},
		{"tools/call", func(c *Client) error { _, err := c.CallTool(context.Background(), "add", nil); return err }},
		{"resources/read", func(c *Client) error { _, err := c.ReadResource(context.Background(), "file:///a"); return err }},
		{"prompts/get", func(c *Client) error { _, err := c.GetPrompt(context.Background(), "p", nil); return err }},
	}
	asking := map[string]bool{"tools/call": true, "resources/read": true, "prompts/get": true}

	// serve answers every request of either handshake with a result every call
	// above can read.
	result := func(method string) map[string]any {
		switch method {
		case "server/discover":
			return map[string]any{"resultType": "complete", "supportedVersions": []any{LatestProtocolVersion},
				"capabilities": map[string]any{}, "ttlMs": 600000}
		case "initialize":
			return map[string]any{"protocolVersion": ProtocolV20251125, "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "s", "version": "1"}}
		default:
			return map[string]any{"resultType": "complete", "tools": []any{}, "resources": []any{}, "prompts": []any{},
				"content": []any{}, "contents": []any{}, "messages": []any{}, "isError": false, "ttlMs": 600000}
		}
	}

	type sent struct {
		method string
		token  bool
	}
	read := func(frame string) sent {
		var decoded struct {
			Method string `json:"method"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(frame), &decoded)
		_, token := decoded.Params.Meta["progressToken"]
		return sent{method: decoded.Method, token: token}
	}

	transports := []struct {
		name string
		// asks is whether a long-running request asks for progress over it.
		asks bool
		// run makes every call over a client of the given handshake and
		// returns the frames that were sent.
		run func(t *testing.T, legacy bool) []string
	}{
		{
			name: "a transport that lets progress extend a request",
			asks: true,
			run: func(t *testing.T, legacy bool) []string {
				f := newFakeTransport()
				f.handlers = map[string]fakeHandler{}
				for _, method := range []string{"initialize", "ping", "tools/list", "resources/list", "prompts/list",
					"tools/call", "resources/read", "prompts/get"} {
					f.on(method, func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
						resp, _ := jsonrpc.NewResult(id, result(method))
						return resp
					})
				}
				if !legacy {
					f.on("server/discover", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
						resp, _ := jsonrpc.NewResult(id, result("server/discover"))
						return resp
					})
				}
				c := New(f, testClientInfo())
				for _, call := range calls {
					if err := call.call(c); err != nil {
						t.Fatalf("%s: %v", call.method, err)
					}
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				return append([]string(nil), f.sent...)
			},
		},
		{
			name: "the HTTP transport",
			run: func(t *testing.T, legacy bool) []string {
				endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
					switch {
					case request.httpMethod == http.MethodDelete:
						w.WriteHeader(http.StatusNoContent)
					case len(request.id) == 0:
						w.WriteHeader(http.StatusAccepted)
					case request.method == "server/discover" && legacy:
						w.WriteHeader(http.StatusBadRequest)
					default:
						writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": result(request.method)}))
					}
				})
				c := Web(endpoint.URL)
				for _, call := range calls {
					if err := call.call(c.Client); err != nil {
						t.Fatalf("%s: %v", call.method, err)
					}
				}
				endpoint.mu.Lock()
				defer endpoint.mu.Unlock()
				frames := make([]string, 0, len(endpoint.requests))
				for _, request := range endpoint.requests {
					frames = append(frames, request.body)
				}
				return frames
			},
		},
	}

	for _, transport := range transports {
		for _, legacy := range []bool{false, true} {
			name := transport.name + "/the discovery handshake"
			if legacy {
				name = transport.name + "/the initialize handshake"
			}
			t.Run(name, func(t *testing.T) {
				seen := map[string]bool{}
				for _, frame := range transport.run(t, legacy) {
					got := read(frame)
					if got.method == "" {
						continue
					}
					seen[got.method] = true
					want := transport.asks && asking[got.method]
					if got.token != want {
						t.Errorf("[%s] carries a progress token = %v, want %v", got.method, got.token, want)
					}
				}
				for _, call := range calls {
					// The discovery revision has no ping: its liveness check is
					// a server/discover, which is among the frames read above.
					if call.method == "ping" && !legacy {
						continue
					}
					if !seen[call.method] {
						t.Errorf("[%s] was never sent", call.method)
					}
				}
			})
		}
	}
}

// TestEncodeParamsRefusesUnusableParams covers the encoder's own contract for
// shapes no public call can produce: params that are not an object, metadata
// that is not an object, and metadata a caller layers on top of the protocol
// keys. The public surface builds every params member itself, so these have no
// wire-level twin.
func TestEncodeParamsRefusesUnusableParams(t *testing.T) {
	p := newProtocol(newScriptedTransport(), schema.NewImplementation("Acme MCP App", "9.9.9"))

	tests := []struct {
		name    string
		params  any
		version ProtocolVersion
		// want is the exact encoded params when the encoding succeeds.
		want string
		// wantErr is a substring of the error the encoding must fail with.
		wantErr string
	}{
		{
			name: "metadata a caller supplies is layered on top of the protocol keys",
			params: map[string]any{"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": "2999-01-01",
				"com.example/trace":                       "abc",
			}},
			version: LatestProtocolVersion,
			want: `{"_meta":{"com.example/trace":"abc",` +
				`"io.modelcontextprotocol/clientCapabilities":{},` +
				`"io.modelcontextprotocol/clientInfo":{"name":"Acme MCP App","version":"9.9.9"},` +
				`"io.modelcontextprotocol/protocolVersion":"2999-01-01"}}`,
		},
		{
			name:    "params that are not an object are refused",
			params:  []string{"add"},
			version: LatestProtocolVersion,
			wantErr: "unable to encode request params",
		},
		{
			name:    "metadata that is not an object is refused",
			params:  map[string]any{"_meta": "trace"},
			version: LatestProtocolVersion,
			wantErr: "the [_meta] member must be an object",
		},
		{
			name:    "params that cannot be encoded are refused",
			params:  map[string]any{"fn": func() {}},
			version: ProtocolV20251125,
			wantErr: "unable to encode request params",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := p.encodeParams(tc.params, tc.version, nil)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected the encoding to fail with %q, got %s", tc.wantErr, encoded)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("params = %s, want %s", encoded, tc.want)
			}
		})
	}
}

func TestAccessorsBeforeAnyConnection(t *testing.T) {
	c := newScriptedClient(newScriptedTransport())

	if c.DiscoverResult() != nil || c.InitializeResult() != nil {
		t.Fatal("a client that has not connected must report no handshake result")
	}
	if c.Connected() {
		t.Fatal("a client that has not connected must not report a connection")
	}
	// The version is only ever that of a connection: asked of a server that
	// never answers the handshake, it is the failure that is reported, and no
	// version beside it.
	version, err := c.ProtocolVersion(context.Background())
	if err == nil || version != "" {
		t.Fatalf("version = %q err = %v, want none while no connection could be negotiated", version, err)
	}
	if c.Connected() {
		t.Fatal("a handshake that failed must not leave a connection reported")
	}
}

func TestResponseForAnotherRequestIsSkipped(t *testing.T) {
	// The server answers an abandoned exchange before the one in flight; the
	// client must keep reading rather than take the stale frame.
	stale := `{"jsonrpc":"2.0","id":9999,"result":{"supportedVersions":["2026-07-28"],"capabilities":{}}}`
	s := newScriptedTransport(stale, discoverFrame(LatestProtocolVersion))
	c := newScriptedClient(s)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := c.DiscoverResult().Instructions; got != "Be nice." {
		t.Fatalf("the client settled on the stale frame: instructions = %q", got)
	}
}

func TestMalformedFrameEndsTheExchange(t *testing.T) {
	s := newScriptedTransport(`{"jsonrpc":"1.0","id":1,"result":{}}`)
	c := newScriptedClient(s)

	err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected a malformed response to fail the handshake")
	}
	if !strings.Contains(err.Error(), "invalid JSON-RPC response from server") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestClientInfoCanBeReplacedBeforeConnecting(t *testing.T) {
	s := newScriptedTransport(discoverFrame(LatestProtocolVersion))
	c := New(s, schema.Implementation{}).WithClientInfo(schema.NewImplementation("Renamed", "2.0.0"))

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	info, ok := s.frame(t, 0).meta()[MetaClientInfo].(map[string]any)
	if !ok || info["name"] != "Renamed" || info["version"] != "2.0.0" {
		t.Fatalf("client info on the wire = %v", s.frame(t, 0).meta()[MetaClientInfo])
	}
	if c.ClientInfo().Name != "Renamed" {
		t.Fatalf("client info = %+v", c.ClientInfo())
	}
}

func TestDefaultClientInfoIsUsedWhenNoneIsGiven(t *testing.T) {
	s := newScriptedTransport(discoverFrame(LatestProtocolVersion))
	c := New(s, schema.Implementation{})

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	raw, err := json.Marshal(s.frame(t, 0).meta()[MetaClientInfo])
	if err != nil {
		t.Fatalf("marshal client info: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decode client info: %v", err)
	}
	if info["name"] != defaultClientInfo().Name || info["version"] != defaultClientInfo().Version {
		t.Fatalf("client info on the wire = %v", info)
	}
}

// TestClientCapabilitiesTravelWhereTheyCanBeServed asserts a declared
// capability reaches a server that would ask for its inputs as part of a
// result, and no further. An initialize-era server asks by sending the client a
// request of its own; this client answers any such request with
// method-not-found, so it declares nothing there however the caller set it.
func TestClientCapabilitiesTravelWhereTheyCanBeServed(t *testing.T) {
	declared := map[string]any{
		"elicitation": map[string]any{},
		"roots":       map[string]any{"listChanged": true},
	}

	tests := []struct {
		name string
		// handshake is the script that settles the connection.
		handshake []string
		// read returns the capabilities the handshake frame carried.
		read func(*testing.T, *scriptedTransport) any
		want map[string]any
	}{
		{
			name:      "a discovery era handshake states them in the metadata",
			handshake: []string{discoverFrame(LatestProtocolVersion)},
			read: func(t *testing.T, s *scriptedTransport) any {
				return s.frame(t, 0).meta()[MetaClientCapabilities]
			},
			want: declared,
		},
		{
			name:      "an initialize era handshake declares none",
			handshake: []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125)},
			read: func(t *testing.T, s *scriptedTransport) any {
				return s.frame(t, 1).Params["capabilities"]
			},
			want: map[string]any{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(tc.handshake...)
			c := newScriptedClient(s).WithClientCapabilities(declared)

			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			if got := tc.read(t, s); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("client capabilities on the wire = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestAnInitializeEraServerIsRefusedTheInputsItAsksFor is the reason the
// declaration stops at the era boundary: a server settled through initialize
// asks for an input with a request of its own, and this client answers every
// request but a ping with method-not-found. Declaring a capability there would
// invite the request it has to refuse.
func TestAnInitializeEraServerIsRefusedTheInputsItAsksFor(t *testing.T) {
	// The tool call is the third request of the connection (the discovery
	// probe, then initialize), so its result is addressed to id 3: the server
	// interleaves its own request before answering it.
	s := newScriptedTransport(
		methodNotFoundFrame(),
		initializeFrame(ProtocolV20251125),
		`{"jsonrpc":"2.0","id":"srv-1","method":"elicitation/create","params":{"message":"who are you?"}}`,
		`{"jsonrpc":"2.0","id":3,"result":{"content":[],"isError":false}}`,
	)
	c := newScriptedClient(s).WithClientCapabilities(map[string]any{"elicitation": map[string]any{}})

	if _, err := c.CallTool(context.Background(), "add", nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	// The answer follows the four frames the connection cost: the probe, the
	// initialize request, the notification that settles it, and the call.
	if got := s.frame(t, 4).Method; got != "" {
		t.Fatalf("frame 4 method = %q, want the answer to the server's request", got)
	}
	if got := s.rawMember(t, 4, "id"); got != `"srv-1"` {
		t.Fatalf("the answer is addressed to %s, want \"srv-1\"", got)
	}
	var refusal jsonrpc.Error
	if err := json.Unmarshal([]byte(s.rawMember(t, 4, "error")), &refusal); err != nil {
		t.Fatalf("frame 4 carries no JSON-RPC error: %v", err)
	}
	if refusal.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("answer to elicitation/create = %+v, want method not found", refusal)
	}
}

// TestClientCapabilitiesDefaultToNone asserts a client that declares nothing
// still states an object, which is what the metadata requires of the member.
func TestClientCapabilitiesDefaultToNone(t *testing.T) {
	s := newScriptedTransport(discoverFrame(LatestProtocolVersion))
	c := newScriptedClient(s).WithClientCapabilities(nil)

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	capabilities, ok := s.frame(t, 0).meta()[MetaClientCapabilities].(map[string]any)
	if !ok || len(capabilities) != 0 {
		t.Fatalf("client capabilities = %v, want an empty object", s.frame(t, 0).meta()[MetaClientCapabilities])
	}
}

// TestUnencodableClientCapabilitiesAreReported asserts a capability set that
// cannot go on the wire fails the request rather than quietly advertising less
// than the caller declared.
func TestUnencodableClientCapabilitiesAreReported(t *testing.T) {
	s := newScriptedTransport(discoverFrame(LatestProtocolVersion))
	c := newScriptedClient(s).WithClientCapabilities(map[string]any{"elicitation": func() {}})

	err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected the handshake to report the capabilities it cannot encode")
	}
	if !strings.Contains(err.Error(), "client capabilities cannot be encoded") {
		t.Fatalf("error = %q", err.Error())
	}
	if got := s.methods(); len(got) != 0 {
		t.Fatalf("the handshake reached the wire: %v", got)
	}
}
