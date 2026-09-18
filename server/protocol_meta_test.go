package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// protocolMeta is the params._meta bag a discovery-handshake client sends on
// every request.
const protocolMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// modernRequest builds a discovery-handshake request for the given method with
// the supplied extra params (which may be empty).
func modernRequest(id int, method string, extra ...string) string {
	params := append([]string{protocolMeta}, extra...)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s}}`, id, method, strings.Join(params, ","))
}

// errorOf decodes a HandleResult that must be an error response.
func errorOf(t *testing.T, res server.HandleResult) *jsonrpc.Error {
	t.Helper()
	if !res.HasResponse || res.Response == nil {
		t.Fatal("expected a response")
	}
	if res.Response.Error == nil {
		t.Fatalf("expected an error response, got result %s", res.Response.Result)
	}
	return res.Response.Error
}

// TestProtocolMetaRejectsMalformedMetadata drives every shape of broken
// protocol metadata through the wire. Each one must be refused with
// InvalidParams naming the member at fault, because a client cannot fix what
// the server will not name.
func TestProtocolMetaRejectsMalformedMetadata(t *testing.T) {
	const versionKey = "io.modelcontextprotocol/protocolVersion"
	const capabilitiesKey = "io.modelcontextprotocol/clientCapabilities"

	tests := []struct {
		name string
		meta string
		want string
	}{
		{"missing version", `{"` + capabilitiesKey + `":{}}`, versionKey},
		{"missing capabilities", `{"` + versionKey + `":"2026-07-28"}`, capabilitiesKey},
		{"null version", `{"` + versionKey + `":null,"` + capabilitiesKey + `":{}}`, versionKey},
		{"numeric version", `{"` + versionKey + `":123,"` + capabilitiesKey + `":{}}`, versionKey},
		{"object version", `{"` + versionKey + `":{},"` + capabilitiesKey + `":{}}`, versionKey},
		{"string capabilities", `{"` + versionKey + `":"2026-07-28","` + capabilitiesKey + `":"nope"}`, capabilitiesKey},
		{"list capabilities", `{"` + versionKey + `":"2026-07-28","` + capabilitiesKey + `":["elicitation"]}`, capabilitiesKey},
		{"null capabilities", `{"` + versionKey + `":"2026-07-28","` + capabilitiesKey + `":null}`, capabilitiesKey},
		{"version reported before capabilities", `{"` + versionKey + `":7,"` + capabilitiesKey + `":7}`, versionKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":`+tt.meta+`}}`)
			rpcErr := errorOf(t, res)
			if rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("code = %d, want %d", rpcErr.Code, jsonrpc.CodeInvalidParams)
			}
			want := "Invalid params: The request [_meta] is missing the required [" + tt.want + "] member."
			if rpcErr.Message != want {
				t.Fatalf("message = %q, want %q", rpcErr.Message, want)
			}
			if rpcErr.Data != nil {
				t.Fatalf("an invalid-params error carries no data, got %v", rpcErr.Data)
			}
		})
	}
}

// TestProtocolMetaAcceptsEmptyArrayCapabilities asserts the one lenience in the
// capabilities check: an empty JSON array means the same thing as an empty
// object (no capabilities), and some encoders emit it that way.
func TestProtocolMetaAcceptsEmptyArrayCapabilities(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":[]}}}`)
	if res.Response.Error != nil {
		t.Fatalf("empty-array capabilities rejected: %+v", res.Response.Error)
	}
}

// TestUnsupportedProtocolVersion asserts a well-formed request naming a version
// the server does not speak is refused with the dedicated code and told which
// versions it may retry with.
func TestUnsupportedProtocolVersion(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}}`)

	if code := errorOf(t, res).Code; code != -32022 {
		t.Fatalf("code = %d, want -32022", code)
	}

	// Assert the wire form: the client reads the supported list and the
	// rejected value out of the encoded error, not the in-process struct.
	encoded, err := json.Marshal(res.Response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	const want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":"2025-11-25","supported":["2026-07-28"]}}}`
	if string(encoded) != want {
		t.Fatalf("wire form =\n%s\nwant\n%s", encoded, want)
	}
}

// TestUnsupportedProtocolVersionFollowsServerConfiguration asserts the accepted
// set is the server's own advertised list, not a package constant, so a server
// pinned to another revision reports that revision.
func TestUnsupportedProtocolVersionFollowsServerConfiguration(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithProtocolVersions("2027-01-01"))
	res := handle(t, s, modernRequest(1, "tools/list"))

	if code := errorOf(t, res).Code; code != jsonrpc.CodeUnsupportedProtocolVersion {
		t.Fatalf("code = %d", code)
	}
	encoded, err := json.Marshal(res.Response.Error)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	const want = `{"code":-32022,"message":"Unsupported protocol version","data":{"requested":"2026-07-28","supported":["2027-01-01"]}}`
	if string(encoded) != want {
		t.Fatalf("wire form =\n%s\nwant\n%s", encoded, want)
	}
}

// TestLegacyRequestsAreExempt asserts that a request declaring neither protocol
// member is served without any metadata requirement: legacy clients predate it.
func TestLegacyRequestsAreExempt(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no params", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"empty params", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{"unrelated meta", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"progressToken":7}}}`},
		{"meta is not an object", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":"nope"}}`},
		{"legacy initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`},
		{"legacy ping", `{"jsonrpc":"2.0","id":1,"method":"ping"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, tt.raw)
			if res.Response.Error != nil {
				t.Fatalf("legacy request refused: %+v", res.Response.Error)
			}
		})
	}
}

// TestDeclaringEitherMemberDemandsBoth asserts the modern/legacy boundary:
// naming one of the two protocol members, even as null, opts the request into
// full validation rather than leaving it exempt.
func TestDeclaringEitherMemberDemandsBoth(t *testing.T) {
	tests := []struct {
		name string
		meta string
	}{
		{"version alone", `{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`},
		{"capabilities alone", `{"io.modelcontextprotocol/clientCapabilities":{}}`},
		{"version present but null", `{"io.modelcontextprotocol/protocolVersion":null}`},
		{"capabilities present but null", `{"io.modelcontextprotocol/clientCapabilities":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":`+tt.meta+`}}`)
			if rpcErr := errorOf(t, res); rpcErr.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("code = %d, want %d", rpcErr.Code, jsonrpc.CodeInvalidParams)
			}
		})
	}
}

// TestProtocolMetaValidatedBeforeDispatch asserts the check runs ahead of
// method lookup: a request made under an unsupported version is refused for
// that reason even when the method it names does not exist, so a client is
// never told to fix the wrong thing.
func TestProtocolMetaValidatedBeforeDispatch(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"no/such/method","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if code := errorOf(t, res).Code; code != jsonrpc.CodeUnsupportedProtocolVersion {
		t.Fatalf("code = %d, want %d", code, jsonrpc.CodeUnsupportedProtocolVersion)
	}
}

// TestUnknownMethodUnderValidMetadata asserts a well-formed modern request for
// an unknown method still reports method-not-found.
func TestUnknownMethodUnderValidMetadata(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, modernRequest(9, "unknown/method"))
	rpcErr := errorOf(t, res)
	if rpcErr.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("code = %d", rpcErr.Code)
	}
	if rpcErr.Message != "The method [unknown/method] was not found." {
		t.Fatalf("message = %q", rpcErr.Message)
	}
}

// TestNotificationsSkipProtocolValidation asserts a notification is never
// answered, even when its metadata would fail a request's validation: a
// notification has no id to correlate a reply to.
func TestNotificationsSkipProtocolValidation(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01"}}}`), "")
	if res.HasResponse {
		t.Fatalf("a notification must produce no reply, got %+v", res.Response)
	}
}

// TestInspectMessage covers the view a transport takes of a raw message before
// handling it, including the inputs it must refuse to classify.
func TestInspectMessage(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantOK       bool
		wantMethod   string
		wantLegacy   bool
		wantVersion  string
		wantName     string
		wantRequires bool
	}{
		{
			name: "modern tool call", raw: modernRequest(1, "tools/call", `"name":"say-hi"`),
			wantOK: true, wantMethod: "tools/call", wantVersion: "2026-07-28", wantName: "say-hi", wantRequires: true,
		},
		{
			name:   "modern resource read uses the uri as its name",
			raw:    modernRequest(1, "resources/read", `"uri":"file://a.txt"`),
			wantOK: true, wantMethod: "resources/read", wantVersion: "2026-07-28",
			wantName: "file://a.txt", wantRequires: true,
		},
		{
			name: "modern prompt get", raw: modernRequest(1, "prompts/get", `"name":"review"`),
			wantOK: true, wantMethod: "prompts/get", wantVersion: "2026-07-28", wantName: "review", wantRequires: true,
		},
		{
			name: "list needs no name", raw: modernRequest(1, "tools/list"),
			wantOK: true, wantMethod: "tools/list", wantVersion: "2026-07-28",
		},
		{
			name: "legacy request", raw: `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
			wantOK: true, wantMethod: "tools/list", wantLegacy: true,
		},
		{
			name: "non-string name is not reported", raw: modernRequest(1, "tools/call", `"name":123`),
			wantOK: true, wantMethod: "tools/call", wantVersion: "2026-07-28", wantRequires: true,
		},
		{
			name:   "non-string version is not reported",
			raw:    `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":7,"io.modelcontextprotocol/clientCapabilities":{}}}}`,
			wantOK: true, wantMethod: "tools/list",
		},
		{
			name: "params is not an object", raw: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}`,
			wantOK: true, wantMethod: "tools/call", wantLegacy: true, wantRequires: true,
		},
		{name: "notification", raw: `{"jsonrpc":"2.0","method":"tools/list"}`},
		{name: "null id", raw: `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`},
		{name: "missing method", raw: `{"jsonrpc":"2.0","id":1}`},
		{name: "non-string method", raw: `{"jsonrpc":"2.0","id":1,"method":7}`},
		// A null method decodes into the empty string without error, so it has
		// to be refused explicitly: reported as a request it would be answered
		// with a header complaint about a body the server rejects outright.
		{name: "null method", raw: `{"jsonrpc":"2.0","id":1,"method":null}`},
		{
			name: "null method on a body that states protocol metadata",
			raw:  `{"jsonrpc":"2.0","id":1,"method":null,"params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		},
		// An id of a type the specification forbids in a response cannot
		// correlate a reply, so such a message is not the guard's to answer.
		{name: "object id", raw: `{"jsonrpc":"2.0","id":{},"method":"tools/list"}`},
		{name: "array id", raw: `{"jsonrpc":"2.0","id":[1],"method":"tools/list"}`},
		{name: "boolean id", raw: `{"jsonrpc":"2.0","id":true,"method":"tools/list"}`},
		{name: "malformed json", raw: `{"jsonrpc":`},
		{name: "not an object", raw: `[1,2,3]`},
		{name: "empty", raw: ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := server.InspectMessage([]byte(tt.raw))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if info.Method != tt.wantMethod {
				t.Errorf("Method = %q, want %q", info.Method, tt.wantMethod)
			}
			if info.Legacy != tt.wantLegacy {
				t.Errorf("Legacy = %v, want %v", info.Legacy, tt.wantLegacy)
			}
			if info.ProtocolVersion != tt.wantVersion {
				t.Errorf("ProtocolVersion = %q, want %q", info.ProtocolVersion, tt.wantVersion)
			}
			if info.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", info.Name, tt.wantName)
			}
			if info.RequiresName != tt.wantRequires {
				t.Errorf("RequiresName = %v, want %v", info.RequiresName, tt.wantRequires)
			}
		})
	}
}

// TestInspectMessagePreservesIDForm asserts the id is handed back in its
// original JSON form, so a reply correlates to the call whatever type the
// client used for the id.
func TestInspectMessagePreservesIDForm(t *testing.T) {
	tests := []struct{ raw, want string }{
		{modernRequest(7, "tools/list"), "7"},
		{`{"jsonrpc":"2.0","id":"abc","method":"tools/list","params":{}}`, `"abc"`},
		{`{"jsonrpc":"2.0","id":1.5,"method":"tools/list","params":{}}`, "1.5"},
	}
	for _, tt := range tests {
		info, ok := server.InspectMessage([]byte(tt.raw))
		if !ok {
			t.Fatalf("InspectMessage(%s) not ok", tt.raw)
		}
		if string(info.ID) != tt.want {
			t.Fatalf("ID = %s, want %s", info.ID, tt.want)
		}
	}
}

// TestConcurrentModernRequests drives many discovery-handshake requests through
// one server at once. The server is shared by every connection, so metadata
// validation and the result envelope must not write to any state shared across
// requests.
func TestConcurrentModernRequests(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithTools(addTool()))

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			raw := modernRequest(id, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)
			res := s.Handle(context.Background(), []byte(raw), "")
			if res.Response == nil || res.Response.Error != nil {
				errs <- fmt.Errorf("worker %d: %+v", id, res.Response)
				return
			}
			var result map[string]any
			if err := json.Unmarshal(res.Response.Result, &result); err != nil {
				errs <- fmt.Errorf("worker %d: %w", id, err)
				return
			}
			if result["resultType"] != "complete" {
				errs <- fmt.Errorf("worker %d: resultType = %v", id, result["resultType"])
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
