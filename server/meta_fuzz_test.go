package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// FuzzInspectMessage drives the inspector that runs over every raw HTTP body
// before anything else has looked at it.
//
// Two invariants hold. It must never panic, because it is the first code an
// untrusted body reaches. And it must classify a message exactly as the server
// does: the HTTP header guard skips a message it reports as not-a-request or as
// legacy, so a message the inspector called legacy while the server validates
// its protocol metadata would reach a handler with its mirrored headers
// unchecked.
func FuzzInspectMessage(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":"a","method":"resources/read","params":{"uri":"file://a"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":null}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":[]}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"","_meta":{"io.modelcontextprotocol/protocolVersion":"","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":123}}`,
		// Valid JSON numbers no Go value can hold: reading params into Go values
		// fails on them, so a classifier that decoded that way would call these
		// modern requests legacy and wave their headers through unchecked.
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"overflow":1e10000,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","overflow":-1e10000,"arguments":{"a":1e10000},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"overflow":1e10000}}}`,
		// The two methods the discovery handshake introduced, stating none of
		// its metadata: they have no earlier form, so neither is a legacy
		// request and neither may be exempted from the rules of that handshake.
		`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`,
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":[]}}`,
		`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"_meta":{"progressToken":1}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":"nope"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`,
		`{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":null}`,
		`{"jsonrpc":"2.0","id":1,"method":null,"params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":{},"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":[1],"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":true,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"a","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"x"}},"params":{}}`,
		`[]`,
		`{`,
		``,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		info, ok := InspectMessage(raw)

		req, parsedID, parseErr := jsonrpc.ParseRequest(raw)

		// A message the guard claims as a request must be one the server can
		// correlate a reply to. The strict parser echoes an id only when the
		// specification permits it in a response and answers with a null id
		// otherwise, so a claimed request whose id the parser refused to echo
		// would be answered by the guard with an error nothing can match up.
		if ok && parsedID.IsNull() {
			t.Fatalf("header guard claims a request the server cannot correlate: %q", raw)
		}
		// It must also carry a string method, the other member that makes a
		// message a request. encoding/json decodes a JSON null into the empty
		// string without complaint, so the token, not the decoded value, is what
		// settles it.
		if ok {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("header guard claims a request for a body that is not an object: %q", raw)
			}
			var method any
			if err := json.Unmarshal(body["method"], &method); err != nil {
				t.Fatalf("header guard claims a request with no [method] member: %q", raw)
			}
			if _, isString := method.(string); !isString {
				t.Fatalf("header guard claims a request whose [method] is %s: %q", body["method"], raw)
			}
		}

		if parseErr != nil || req == nil {
			// The server answers with a parse or invalid-request error and
			// never classifies the message, so there is nothing more to agree on.
			return
		}

		meta, hasMeta := requestMeta(req)
		// The transport reads Legacy as "the server will not hold this message
		// to the rules of the discovery handshake" and skips both the mirrored
		// headers and the status mapping on the strength of it. What the server
		// actually does is therefore the oracle, rather than a second copy of
		// the classification rule: a message called legacy that the server then
		// refuses for its protocol metadata is one answered with an error the
		// transport dressed as a success, with its headers never read.
		exempted := validateProtocolMeta(newTestContext(), req) == nil

		if !ok {
			if exempted {
				return
			}
			t.Fatalf("header guard would skip a request the server refuses on protocol terms: %q", raw)
		}
		if info.Legacy && !exempted {
			t.Fatalf("classified legacy, but the server refuses the request on protocol terms: %q", raw)
		}
		// The other direction, so the guard cannot buy that agreement by
		// calling everything modern: a body declaring neither reserved _meta
		// key is a legacy request, except for the two methods revision
		// 2026-07-28 introduced, which no earlier client can be calling.
		discoveryOnly := req.Method == "server/discover" || req.Method == "subscriptions/listen"
		if !info.Legacy && isLegacyMeta(meta, hasMeta) && !discoveryOnly {
			t.Fatalf("classified modern, though the body declares no protocol metadata: %q", raw)
		}
		if info.Method != req.Method {
			t.Fatalf("method %q, server says %q: %q", info.Method, req.Method, raw)
		}
		if info.RequiresName != (namedParamKey(req.Method) != "") {
			t.Fatalf("requiresName=%v for method %q: %q", info.RequiresName, req.Method, raw)
		}
		if got, want := bytes.TrimSpace(info.ID), bytes.TrimSpace(req.ID.Raw()); !bytes.Equal(got, want) {
			t.Fatalf("id token %q, server echoes %q: %q", got, want, raw)
		}
		// A stated value must be exactly what the server would read back out of
		// the same body, so a mirrored header is compared against the member the
		// handler will act on.
		if v, isString := stringMember(meta[MetaKeyProtocolVersion]); isString != info.HasProtocolVersion || (isString && v != info.ProtocolVersion) {
			t.Fatalf("protocol version %q/%v, server reads %q/%v: %q", info.ProtocolVersion, info.HasProtocolVersion, v, isString, raw)
		}
	})
}
