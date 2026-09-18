package server_test

import (
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// A JSON number valid on the wire that no Go numeric type can hold. Reading a
// params object into Go values fails on it, and every rule that reads protocol
// metadata has to survive that: a request carrying one still declares whatever
// its _meta says, and is validated on those terms.
const unreadableNumber = "1e10000"

// TestProtocolMetaSurvivesUnreadableNumbers asserts a member no Go value can
// hold does not disable the protocol-metadata rules. The offending member is
// unrelated to the metadata; if its presence made the whole params object
// unreadable, the request would look as though it declared no protocol version
// and be exempted from validation altogether.
func TestProtocolMetaSurvivesUnreadableNumbers(t *testing.T) {
	const versionKey = "io.modelcontextprotocol/protocolVersion"
	const capabilitiesKey = "io.modelcontextprotocol/clientCapabilities"

	tests := []struct {
		name    string
		params  string
		code    int
		message string
	}{
		{
			name:    "unsupported version beside an unreadable number",
			params:  `{"overflow":` + unreadableNumber + `,"_meta":{"` + versionKey + `":"1999-01-01","` + capabilitiesKey + `":{}}}`,
			code:    jsonrpc.CodeUnsupportedProtocolVersion,
			message: "Unsupported protocol version",
		},
		{
			name:    "missing capabilities beside an unreadable number",
			params:  `{"overflow":` + unreadableNumber + `,"_meta":{"` + versionKey + `":"2026-07-28"}}`,
			code:    jsonrpc.CodeInvalidParams,
			message: "Invalid params: The request [_meta] is missing the required [" + capabilitiesKey + "] member.",
		},
		{
			name:    "unreadable number inside the metadata bag",
			params:  `{"_meta":{"` + versionKey + `":"1999-01-01","` + capabilitiesKey + `":{},"tokens":` + unreadableNumber + `}}`,
			code:    jsonrpc.CodeUnsupportedProtocolVersion,
			message: "Unsupported protocol version",
		},
		{
			name:    "unreadable number as the declared version",
			params:  `{"_meta":{"` + versionKey + `":` + unreadableNumber + `,"` + capabilitiesKey + `":{}}}`,
			code:    jsonrpc.CodeInvalidParams,
			message: "Invalid params: The request [_meta] is missing the required [" + versionKey + "] member.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping","params":`+tt.params+`}`)
			rpcErr := errorOf(t, res)
			if rpcErr.Code != tt.code {
				t.Fatalf("code = %d, want %d (message %q)", rpcErr.Code, tt.code, rpcErr.Message)
			}
			if rpcErr.Message != tt.message {
				t.Fatalf("message = %q, want %q", rpcErr.Message, tt.message)
			}
		})
	}
}

// TestArgumentsShapeSurvivesUnreadableNumbers asserts the [arguments] rule is
// enforced on a request that also carries a number no Go value can hold: a tool
// must not run with an empty bag because a sibling member could not be decoded.
func TestArgumentsShapeSurvivesUnreadableNumbers(t *testing.T) {
	s := argumentsServer(t)
	res := handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","overflow":`+
		unreadableNumber+`,"arguments":"nope"}}`)
	rpcErr := errorOf(t, res)
	if rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", rpcErr.Code, jsonrpc.CodeInvalidParams)
	}
	const want = "Invalid params: The [arguments] member must be an object."
	if rpcErr.Message != want {
		t.Fatalf("message = %q, want %q", rpcErr.Message, want)
	}
}

// TestAcceptedRequestCarryingAnUnreadableNumber asserts the rules stay a filter
// and not a wall: a well-formed discovery request is served even when one of its
// members cannot be read into a Go value.
func TestAcceptedRequestCarryingAnUnreadableNumber(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, modernRequest(1, "ping", `"overflow":`+unreadableNumber))
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("request refused: %+v", res.Response)
	}
}

// TestInspectMessageSurvivesUnreadableNumbers asserts the inspector the HTTP
// header guard runs on the raw body reads the same declaration the server does.
// Were an unreadable member to make it report "legacy", the guard would skip
// every mirrored-header check for that request.
func TestInspectMessageSurvivesUnreadableNumbers(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","overflow":` + unreadableNumber +
		`,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`

	info, ok := server.InspectMessage([]byte(raw))
	if !ok {
		t.Fatal("InspectMessage did not recognise the message as a request")
	}
	if info.Legacy {
		t.Fatal("a request declaring a protocol version was classified as legacy")
	}
	if !info.HasProtocolVersion || info.ProtocolVersion != "2026-07-28" {
		t.Fatalf("protocol version = %q (stated %v), want 2026-07-28", info.ProtocolVersion, info.HasProtocolVersion)
	}
	if !info.HasName || info.Name != "add" {
		t.Fatalf("name = %q (stated %v), want add", info.Name, info.HasName)
	}
}
