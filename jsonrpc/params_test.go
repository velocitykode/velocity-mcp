package jsonrpc

import (
	"encoding/json"
	"testing"
)

// paramsCase is one inbound message and the verdict its [params] member earns.
type paramsCase struct {
	name     string
	params   string
	wantCode int
}

// paramsCases are shared between the request and the notification parser: the
// [params] member has the same shape rules in both.
var paramsCases = []paramsCase{
	{"object", `{"a":1}`, 0},
	{"empty object", `{}`, 0},
	{"empty array stands in for an empty object", `[]`, 0},
	{"nested object", `{"_meta":{"k":[1,2]}}`, 0},
	{"non-empty array", `[1,2]`, CodeInvalidParams},
	{"array of objects", `[{"a":1}]`, CodeInvalidParams},
	{"string", `"invalid"`, CodeInvalidParams},
	{"number", `42`, CodeInvalidParams},
	{"boolean", `true`, CodeInvalidParams},
	{"null", `null`, CodeInvalidParams},
	{"array with whitespace only", "[  ]", 0},
}

// TestParseRequestParamsShape asserts a request's [params] must be a structured
// value. A scalar or a list would silently read back as "no parameters", so a
// mistyped call has to be refused rather than served as an empty one.
func TestParseRequestParamsShape(t *testing.T) {
	for _, tt := range paramsCases {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"jsonrpc":"2.0","id":272,"method":"tools/call","params":` + tt.params + `}`)
			req, id, err := ParseRequest(raw)

			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %+v", err)
				}
				if req.Method != "tools/call" {
					t.Fatalf("method = %q", req.Method)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", err.Code, tt.wantCode)
			}
			if err.Message != "Invalid params: The [params] member must be an object." {
				t.Fatalf("message = %q", err.Message)
			}
			// The id is recovered so the error response correlates to the call.
			if string(id.Raw()) != "272" {
				t.Fatalf("id = %s, want 272", id.Raw())
			}
		})
	}
}

// TestParseNotificationParamsShape asserts the same rule on a notification.
func TestParseNotificationParamsShape(t *testing.T) {
	for _, tt := range paramsCases {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":` + tt.params + `}`)
			ntf, err := ParseNotification(raw)

			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %+v", err)
				}
				if ntf.Method != "notifications/initialized" {
					t.Fatalf("method = %q", ntf.Method)
				}
				return
			}
			if err == nil || err.Code != tt.wantCode {
				t.Fatalf("error = %+v, want code %d", err, tt.wantCode)
			}
		})
	}
}

// TestAbsentParamsIsValid asserts omitting [params] entirely stays legal: the
// shape rule applies to a member that is present, not to one that is not.
func TestAbsentParamsIsValid(t *testing.T) {
	if _, _, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("request without params rejected: %+v", err)
	}
	if _, err := ParseNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatalf("notification without params rejected: %+v", err)
	}
}

// TestParamsShapeCheckedAfterTheEnvelope asserts the envelope members are
// reported first: a message with both a bad version and bad params is told
// about the version, which is the more fundamental fault.
func TestParamsShapeCheckedAfterTheEnvelope(t *testing.T) {
	_, _, err := ParseRequest([]byte(`{"jsonrpc":"1.0","id":1,"method":"ping","params":"nope"}`))
	if err == nil || err.Code != CodeInvalidRequest {
		t.Fatalf("error = %+v, want an invalid-request error", err)
	}

	_, _, err = ParseRequest([]byte(`{"jsonrpc":"2.0","id":1,"params":"nope"}`))
	if err == nil || err.Code != CodeInvalidRequest {
		t.Fatalf("error = %+v, want an invalid-request error", err)
	}
}

// TestMCPErrorCodes pins the numeric protocol error codes. They are a wire
// contract clients branch on, so a value drifting would silently change how a
// failure is interpreted.
func TestMCPErrorCodes(t *testing.T) {
	tests := []struct {
		name string
		code int
		want int
	}{
		{"parse error", CodeParseError, -32700},
		{"invalid request", CodeInvalidRequest, -32600},
		{"method not found", CodeMethodNotFound, -32601},
		{"invalid params", CodeInvalidParams, -32602},
		{"internal error", CodeInternalError, -32603},
		{"header mismatch", CodeHeaderMismatch, -32020},
		{"missing client capability", CodeMissingRequiredClientCapability, -32021},
		{"unsupported protocol version", CodeUnsupportedProtocolVersion, -32022},
		{"resource not found", CodeResourceNotFound, -32002},
	}
	for _, tt := range tests {
		if tt.code != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.code, tt.want)
		}
	}
}

// FuzzParseRequest drives the request parser, which reads untrusted bytes off
// the wire. It must never panic, and must never hand back a request together
// with an error or a request whose id could not be used.
func FuzzParseRequest(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`,
		`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":[]}`,
		`{"jsonrpc":"2.0","id":null,"method":"x","params":null}`,
		`{"jsonrpc":"2.0","id":1,"method":7}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"params":{}}`,
		`[]`,
		`{`,
		``,
		`{"jsonrpc":"2.0","id":1,"method":"a"} {"jsonrpc":"2.0","id":2,"method":"b"}`,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"\\u0000\"}",
		// Numbers whose syntax is valid but whose value no decoder can read:
		// they pass the pass that fills the envelope and fail the one that
		// reads the member.
		`{"jsonrpc":"2.0","id":1,"method":1e999}`,
		`{"jsonrpc":"2.0","id":1,"method":[1e999]}`,
		`{"jsonrpc":1e999,"id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1e999,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"n":1e999}}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		req, id, err := ParseRequest(raw)
		if (req == nil) == (err == nil) {
			t.Fatalf("exactly one of request and error must be set: req=%v err=%v", req, err)
		}
		if err != nil {
			if err.Code != CodeParseError && err.Code != CodeInvalidRequest && err.Code != CodeInvalidParams {
				t.Fatalf("unexpected error code %d for %q", err.Code, raw)
			}
			return
		}
		if !id.IsValidRequestID() {
			t.Fatalf("accepted a request with an unusable id: %q", raw)
		}
		// The id token travels back out: a response echoes it, and a handler
		// that correlates a streamed frame with the request marshals it into
		// that frame. An accepted id that is not a JSON token would leave those
		// frames unwritable at the moment of writing them, with the request
		// already accepted, so the parser must never hand one on.
		if !json.Valid(id.Raw()) {
			t.Fatalf("accepted a request whose id token is not JSON: %q (from %q)", id.Raw(), raw)
		}
		if req.JSONRPC != Version {
			t.Fatalf("accepted a request with version %q", req.JSONRPC)
		}
		if len(req.Params) > 0 && !isParamsObject(req.Params) {
			t.Fatalf("accepted params %q", req.Params)
		}
	})
}

// FuzzParseNotification drives the notification parser over untrusted input. A
// notification is never answered, so a panic here would take the connection
// down with no diagnostic reaching the peer.
func FuzzParseNotification(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"x","params":{}}`,
		`{"jsonrpc":"2.0","method":"x","params":[1]}`,
		`{"jsonrpc":"2.0"}`,
		`{}`,
		`null`,
		`{"jsonrpc":"2.0","method":"x","params":`,
		// Numbers whose syntax is valid but whose value no decoder can read.
		`{"jsonrpc":"2.0","method":1e999}`,
		`{"jsonrpc":1e999,"method":"x"}`,
		`{"jsonrpc":"2.0","method":"x","params":{"n":-1e999}}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		ntf, err := ParseNotification(raw)
		if (ntf == nil) == (err == nil) {
			t.Fatalf("exactly one of notification and error must be set for %q", raw)
		}
		if err != nil {
			if err.Code != CodeParseError && err.Code != CodeInvalidRequest && err.Code != CodeInvalidParams {
				t.Fatalf("unexpected error code %d for %q", err.Code, raw)
			}
			return
		}
		// An accepted notification is normalized: its version is the only one
		// the protocol allows and its params, if present, are structured. The
		// method may be any JSON string, empty included, which the protocol
		// permits and a handler rejects by name.
		if ntf.JSONRPC != Version {
			t.Fatalf("accepted a notification with version %q", ntf.JSONRPC)
		}
		if len(ntf.Params) > 0 && !isParamsObject(ntf.Params) {
			t.Fatalf("accepted params %q", ntf.Params)
		}
	})
}
