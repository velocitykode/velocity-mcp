package jsonrpc

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestParseRequest(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantErr    bool
		wantCode   int
		wantMsg    string
		wantMethod string
		wantIDStr  string
		wantParams string
	}{
		{
			name:       "valid string id",
			input:      `{"jsonrpc":"2.0","id":"1","method":"ping","params":{}}`,
			wantMethod: "ping",
			wantIDStr:  "1",
			wantParams: `{}`,
		},
		{
			name:       "valid int id no params",
			input:      `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`,
			wantMethod: "tools/list",
			wantIDStr:  "7",
			wantParams: "",
		},
		{
			name:     "malformed json",
			input:    `{not json`,
			wantErr:  true,
			wantCode: CodeParseError,
			wantMsg:  msgParseError,
		},
		{
			name:     "trailing data",
			input:    `{"jsonrpc":"2.0","id":1,"method":"ping"}{"x":1}`,
			wantErr:  true,
			wantCode: CodeParseError,
			wantMsg:  msgParseError,
		},
		{
			name:     "missing id",
			input:    `{"jsonrpc":"2.0","method":"ping"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgInvalidID,
		},
		{
			name:     "null id",
			input:    `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgInvalidID,
		},
		{
			name:     "bool id",
			input:    `{"jsonrpc":"2.0","id":true,"method":"ping"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgInvalidID,
		},
		{
			name:      "wrong version",
			input:     `{"jsonrpc":"1.0","id":4,"method":"ping"}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgInvalidVersion,
			wantIDStr: "4",
		},
		{
			name:      "missing version",
			input:     `{"id":4,"method":"ping"}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgInvalidVersion,
			wantIDStr: "4",
		},
		{
			name:      "missing method",
			input:     `{"jsonrpc":"2.0","id":4}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgMissingMethodReq,
			wantIDStr: "4",
		},
		{
			name:      "non-string method",
			input:     `{"jsonrpc":"2.0","id":4,"method":123}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgMissingMethodReq,
			wantIDStr: "4",
		},
		{
			// A null method is the case a permissive decoder gets wrong: JSON
			// null leaves a string destination untouched without reporting an
			// error, so the message would be read as a request naming the
			// empty method and answered with a method-not-found instead of the
			// invalid-request the specification calls for.
			name:      "null method",
			input:     `{"jsonrpc":"2.0","id":4,"method":null}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgMissingMethodReq,
			wantIDStr: "4",
		},
		{
			name:      "boolean method",
			input:     `{"jsonrpc":"2.0","id":4,"method":true}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgMissingMethodReq,
			wantIDStr: "4",
		},
		{
			name:      "object method",
			input:     `{"jsonrpc":"2.0","id":4,"method":{"name":"ping"}}`,
			wantErr:   true,
			wantCode:  CodeInvalidRequest,
			wantMsg:   msgMissingMethodReq,
			wantIDStr: "4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, id, jerr := ParseRequest([]byte(tt.input))
			if tt.wantErr {
				if jerr == nil {
					t.Fatalf("expected error, got nil")
				}
				if jerr.Code != tt.wantCode {
					t.Errorf("code = %d, want %d", jerr.Code, tt.wantCode)
				}
				if jerr.Message != tt.wantMsg {
					t.Errorf("msg = %q, want %q", jerr.Message, tt.wantMsg)
				}
				if req != nil {
					t.Errorf("expected nil request on error")
				}
				if tt.wantIDStr != "" && id.String() != tt.wantIDStr {
					t.Errorf("echoed id = %q, want %q", id.String(), tt.wantIDStr)
				}
				return
			}
			if jerr != nil {
				t.Fatalf("unexpected error: %v", jerr)
			}
			if req.Method != tt.wantMethod {
				t.Errorf("method = %q, want %q", req.Method, tt.wantMethod)
			}
			if id.String() != tt.wantIDStr {
				t.Errorf("id = %q, want %q", id.String(), tt.wantIDStr)
			}
			if string(req.Params) != tt.wantParams {
				t.Errorf("params = %q, want %q", req.Params, tt.wantParams)
			}
			if req.JSONRPC != Version {
				t.Errorf("jsonrpc = %q, want %q", req.JSONRPC, Version)
			}
		})
	}
}

func TestParseRequest_EdgeCase_IDEchoedOnVersionError(t *testing.T) {
	// A valid id with a bad version should still echo the id so the caller can
	// correlate the error response.
	_, id, jerr := ParseRequest([]byte(`{"jsonrpc":"x","id":"abc","method":"ping"}`))
	if jerr == nil || jerr.Code != CodeInvalidRequest {
		t.Fatalf("expected invalid request, got %v", jerr)
	}
	if id.String() != "abc" {
		t.Fatalf("id = %q, want abc", id.String())
	}
}

func TestParseRequest_EdgeCase_InvalidIDEchoesNull(t *testing.T) {
	_, id, jerr := ParseRequest([]byte(`{"jsonrpc":"2.0","id":{},"method":"ping"}`))
	if jerr == nil {
		t.Fatalf("expected error")
	}
	if !id.IsNull() {
		t.Fatalf("expected null echoed id, got %q", id.String())
	}
}

func TestParseRequest_ParamsNotAliased(t *testing.T) {
	input := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":1}}`)
	req, _, jerr := ParseRequest(input)
	if jerr != nil {
		t.Fatalf("unexpected error: %v", jerr)
	}
	// Mutating the source buffer must not corrupt the parsed params.
	for i := range input {
		input[i] = 'Z'
	}
	if string(req.Params) != `{"a":1}` {
		t.Fatalf("params aliased the input buffer: %s", req.Params)
	}
}

func TestParseNotification(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantErr    bool
		wantCode   int
		wantMsg    string
		wantMethod string
		wantParams string
	}{
		{
			name:       "valid",
			input:      `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			wantMethod: "notifications/initialized",
		},
		{
			name:       "valid with params",
			input:      `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
			wantMethod: "notifications/cancelled",
			wantParams: `{"requestId":1}`,
		},
		{
			name:     "malformed",
			input:    `nope`,
			wantErr:  true,
			wantCode: CodeParseError,
			wantMsg:  msgParseError,
		},
		{
			name:     "wrong version",
			input:    `{"jsonrpc":"2.1","method":"x"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgInvalidNtfVer,
		},
		{
			name:     "missing version",
			input:    `{"method":"x"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgInvalidNtfVer,
		},
		{
			name:     "missing method",
			input:    `{"jsonrpc":"2.0"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgMissingMethodNtf,
		},
		{
			name:     "non-string method",
			input:    `{"jsonrpc":"2.0","method":42}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgMissingMethodNtf,
		},
		{
			name:     "null method",
			input:    `{"jsonrpc":"2.0","method":null}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
			wantMsg:  msgMissingMethodNtf,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, jerr := ParseNotification([]byte(tt.input))
			if tt.wantErr {
				if jerr == nil {
					t.Fatalf("expected error")
				}
				if jerr.Code != tt.wantCode {
					t.Errorf("code = %d, want %d", jerr.Code, tt.wantCode)
				}
				if jerr.Message != tt.wantMsg {
					t.Errorf("msg = %q, want %q", jerr.Message, tt.wantMsg)
				}
				if n != nil {
					t.Errorf("expected nil notification on error")
				}
				return
			}
			if jerr != nil {
				t.Fatalf("unexpected error: %v", jerr)
			}
			if n.Method != tt.wantMethod {
				t.Errorf("method = %q, want %q", n.Method, tt.wantMethod)
			}
			if string(n.Params) != tt.wantParams {
				t.Errorf("params = %q, want %q", n.Params, tt.wantParams)
			}
		})
	}
}

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   bool
		wantCode  int
		wantIDStr string
		wantErrIn bool
	}{
		{
			name:      "success result",
			input:     `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
			wantIDStr: "1",
		},
		{
			name:      "error result",
			input:     `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"nope"}}`,
			wantIDStr: "2",
			wantErrIn: true,
		},
		{
			name:      "null id error",
			input:     `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse"}}`,
			wantErrIn: true,
		},
		{
			name:     "malformed",
			input:    `{bad`,
			wantErr:  true,
			wantCode: CodeParseError,
		},
		{
			name:     "wrong version",
			input:    `{"jsonrpc":"1.0","id":1,"result":{}}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
		},
		{
			name:     "neither result nor error",
			input:    `{"jsonrpc":"2.0","id":1}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
		},
		{
			name:     "both result and error",
			input:    `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-1,"message":"x"}}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
		},
		{
			name:     "malformed error object",
			input:    `{"jsonrpc":"2.0","id":1,"error":"not an object"}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
		},
		{
			name:      "explicit null error treated as absent",
			input:     `{"jsonrpc":"2.0","id":1,"result":{"ok":true},"error":null}`,
			wantIDStr: "1",
		},
		{
			name:     "non-string version",
			input:    `{"jsonrpc":2.0,"id":1,"result":{}}`,
			wantErr:  true,
			wantCode: CodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, jerr := ParseResponse([]byte(tt.input))
			if tt.wantErr {
				if jerr == nil {
					t.Fatalf("expected error")
				}
				if jerr.Code != tt.wantCode {
					t.Errorf("code = %d, want %d", jerr.Code, tt.wantCode)
				}
				return
			}
			if jerr != nil {
				t.Fatalf("unexpected error: %v", jerr)
			}
			if tt.wantIDStr != "" && resp.ID.String() != tt.wantIDStr {
				t.Errorf("id = %q, want %q", resp.ID.String(), tt.wantIDStr)
			}
			if tt.wantErrIn && resp.Error == nil {
				t.Errorf("expected error in response")
			}
			if !tt.wantErrIn && resp.Error != nil {
				t.Errorf("unexpected error in response: %v", resp.Error)
			}
		})
	}
}

func TestIsNotificationBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    bool
		wantErr bool
	}{
		{"notification", `{"jsonrpc":"2.0","method":"x"}`, true, false},
		{"request with int id", `{"jsonrpc":"2.0","id":1,"method":"x"}`, false, false},
		{"request with string id", `{"jsonrpc":"2.0","id":"abc","method":"x"}`, false, false},
		// A present-but-null id is routed as a notification (no reply): a
		// present-but-null id is treated as absent, so {"id":null,...} becomes
		// a notification.
		{"present-but-null id is a notification", `{"jsonrpc":"2.0","id":null,"method":"x"}`, true, false},
		{"malformed", `{oops`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsNotificationBytes([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsNotificationBytes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStrictUnmarshal_TrailingWhitespaceAllowed(t *testing.T) {
	// Trailing whitespace after a valid value must not be treated as garbage.
	var v map[string]any
	if err := strictUnmarshal([]byte(`{"a":1}   `+"\n"), &v); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
}

func TestParse_Concurrent(t *testing.T) {
	inputs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":1}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"x","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`,
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data := []byte(inputs[n%len(inputs)])
			switch n % 4 {
			case 0:
				if _, _, err := ParseRequest(data); err != nil && n%len(inputs) == 0 {
					t.Errorf("ParseRequest failed: %v", err)
				}
			case 1:
				ParseNotification(data)
			case 2:
				if _, err := IsNotificationBytes(data); err != nil {
					t.Errorf("IsNotificationBytes failed: %v", err)
				}
			case 3:
				ParseResponse(data)
			}
			// Exercise constructors concurrently too.
			if _, err := NewResult(IntID(int64(n)), map[string]any{"n": n}); err != nil {
				t.Errorf("NewResult failed: %v", err)
			}
			_ = NewError(CodeInternalError, "x")
		}(i)
	}
	wg.Wait()
}

func TestRequest_MarshalRoundTrip(t *testing.T) {
	// A parsed request re-marshals to a stable, spec-shaped object.
	req, _, jerr := ParseRequest([]byte(`{"jsonrpc":"2.0","id":5,"method":"ping","params":{"k":"v"}}`))
	if jerr != nil {
		t.Fatalf("parse: %v", jerr)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":5,"method":"ping","params":{"k":"v"}}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}

// TestParseResponseCorrelatesTheID asserts the id a reply carries is preserved
// in the form the peer wrote it, so a client matches the reply to the call it
// made, and that a reply naming no call at all correlates to nothing rather
// than to an id invented here.
func TestParseResponseCorrelatesTheID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantRaw  string
		wantNull bool
	}{
		{name: "integer id", input: `{"jsonrpc":"2.0","id":7,"result":{}}`, wantRaw: "7"},
		{name: "string id", input: `{"jsonrpc":"2.0","id":"abc","result":{}}`, wantRaw: `"abc"`},
		{name: "fractional id", input: `{"jsonrpc":"2.0","id":1.50,"result":{}}`, wantRaw: "1.50"},
		{name: "large id keeps its digits", input: `{"jsonrpc":"2.0","id":12345678901234567890,"result":{}}`, wantRaw: "12345678901234567890"},
		{name: "explicit null id", input: `{"jsonrpc":"2.0","id":null,"result":{}}`, wantRaw: "null", wantNull: true},
		// A reply with no id member at all: the specification allows that only
		// for a failure that could not be correlated, so it stands in as the
		// explicit null id rather than being turned into some other call's id.
		// A result stating no id is refused instead (see
		// TestParseResponseHostileInput).
		{name: "absent id on an error", input: `{"jsonrpc":"2.0","error":{"code":-32700,"message":"parse"}}`, wantRaw: "null", wantNull: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, jerr := ParseResponse([]byte(tt.input))
			if jerr != nil {
				t.Fatalf("unexpected error: %v", jerr)
			}
			if got := string(resp.ID.Raw()); got != tt.wantRaw {
				t.Fatalf("id raw = %q, want %q", got, tt.wantRaw)
			}
			if resp.ID.IsNull() != tt.wantNull {
				t.Fatalf("IsNull = %v, want %v", resp.ID.IsNull(), tt.wantNull)
			}
		})
	}
}

// TestParseResponseHostileInput asserts a reply this parser cannot trust is
// refused rather than half-read. A peer's reply is untrusted input: a response
// object that is neither a result nor an error, or one whose error member is not
// an error object, must never reach a caller as a plausible outcome.
func TestParseResponseHostileInput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantCode int
		wantMsg  string
	}{
		{
			name: "trailing second response", input: `{"jsonrpc":"2.0","id":1,"result":{}} {"jsonrpc":"2.0","id":2,"result":{}}`,
			wantCode: CodeParseError, wantMsg: "Parse error: Invalid JSON was received by the server.",
		},
		{
			name: "trailing token", input: `{"jsonrpc":"2.0","id":1,"result":{}}]`,
			wantCode: CodeParseError, wantMsg: "Parse error: Invalid JSON was received by the server.",
		},
		{
			name: "not an object", input: `[{"jsonrpc":"2.0","id":1,"result":{}}]`,
			wantCode: CodeParseError, wantMsg: "Parse error: Invalid JSON was received by the server.",
		},
		{
			name: "empty input", input: ``,
			wantCode: CodeParseError, wantMsg: "Parse error: Invalid JSON was received by the server.",
		},
		{
			name: "version absent", input: `{"id":1,"result":{}}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Request: The [jsonrpc] member must be exactly [2.0].",
		},
		{
			name: "version is null", input: `{"jsonrpc":null,"id":1,"result":{}}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Request: The [jsonrpc] member must be exactly [2.0].",
		},
		{
			// A version whose number is syntactically valid but too large to
			// read: the envelope decoder accepts the token and only reading its
			// value reports the failure.
			name: "version is a number outside the float range", input: `{"jsonrpc":1e999,"id":1,"result":{}}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Request: The [jsonrpc] member must be exactly [2.0].",
		},
		{
			name: "error member is a list", input: `{"jsonrpc":"2.0","id":1,"error":[1,2]}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Response: malformed [error] object.",
		},
		{
			// A result answers a call, and the specification requires it to
			// carry that call's id; only a failure may state none. Accepting
			// one under a null id would hand a caller a reply it cannot place.
			name: "result with no id member", input: `{"jsonrpc":"2.0","result":{}}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Response: A [result] must carry the [id] of the request it answers.",
		},
		{
			name: "error member is a number", input: `{"jsonrpc":"2.0","id":1,"error":7}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Response: malformed [error] object.",
		},
		{
			name: "error code is not a number", input: `{"jsonrpc":"2.0","id":1,"error":{"code":"nope","message":"x"}}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Response: malformed [error] object.",
		},
		{
			name: "both members, error first", input: `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"x"},"result":{}}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Response: exactly one of [result] or [error] must be present.",
		},
		{
			name: "neither member", input: `{"jsonrpc":"2.0","id":1}`,
			wantCode: CodeInvalidRequest, wantMsg: "Invalid Response: exactly one of [result] or [error] must be present.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, jerr := ParseResponse([]byte(tt.input))
			if resp != nil {
				t.Fatalf("a refused reply still produced a response: %+v", resp)
			}
			if jerr == nil {
				t.Fatal("expected an error")
			}
			if jerr.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", jerr.Code, tt.wantCode)
			}
			if jerr.Message != tt.wantMsg {
				t.Fatalf("message = %q, want %q", jerr.Message, tt.wantMsg)
			}
		})
	}
}

// TestParseNumbersOutsideTheFloatRange asserts a member whose JSON number is
// well formed but too large to read is refused as the invalid message it is,
// rather than read as a member that is absent.
//
// Such a token passes the first decoding pass, which only checks number syntax
// while filling the envelope, and fails on the second, which reads the value
// itself. A parser trusting the first pass would answer a request naming no
// method with a method-not-found error, and one naming no version as if it had
// declared 2.0, so a peer could reach a handler with a frame the parser never
// actually read. The wire strings below are the ones a client sees, spelled out
// here rather than taken from the parser.
func TestParseNumbersOutsideTheFloatRange(t *testing.T) {
	tests := []struct {
		name string
		// input is a request body; the notification form drops its id member.
		input     string
		wantCode  int
		wantReqID string
		wantReq   string
		wantNtf   string
	}{
		{
			name:      "method is a number outside the float range",
			input:     `{"jsonrpc":"2.0","id":1,"method":1e999}`,
			wantCode:  CodeInvalidRequest,
			wantReqID: "1",
			wantReq:   "Invalid Request: The [method] member is required and must be a string.",
			wantNtf:   "Invalid Request: Invalid or missing [method]. Must be a string.",
		},
		{
			name:      "method is a list holding one",
			input:     `{"jsonrpc":"2.0","id":"a","method":[1e999]}`,
			wantCode:  CodeInvalidRequest,
			wantReqID: `"a"`,
			wantReq:   "Invalid Request: The [method] member is required and must be a string.",
			wantNtf:   "Invalid Request: Invalid or missing [method]. Must be a string.",
		},
		{
			name:      "method is an object holding one",
			input:     `{"jsonrpc":"2.0","id":1,"method":{"n":-1e999}}`,
			wantCode:  CodeInvalidRequest,
			wantReqID: "1",
			wantReq:   "Invalid Request: The [method] member is required and must be a string.",
			wantNtf:   "Invalid Request: Invalid or missing [method]. Must be a string.",
		},
		{
			name:      "version is a number outside the float range",
			input:     `{"jsonrpc":1e999,"id":1,"method":"ping"}`,
			wantCode:  CodeInvalidRequest,
			wantReqID: "1",
			wantReq:   "Invalid Request: The [jsonrpc] member must be exactly [2.0].",
			wantNtf:   "Invalid Request: Invalid JSON-RPC version. Must be [2.0].",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, id, jerr := ParseRequest([]byte(tt.input))
			if req != nil {
				t.Fatalf("a refused request still produced %+v", req)
			}
			if jerr == nil {
				t.Fatal("expected an error")
			}
			if jerr.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", jerr.Code, tt.wantCode)
			}
			if jerr.Message != tt.wantReq {
				t.Fatalf("message = %q, want %q", jerr.Message, tt.wantReq)
			}
			// The id is recovered before the failing member is read, so the
			// client can match the error to the call it made.
			if got := string(id.Raw()); got != tt.wantReqID {
				t.Fatalf("id = %s, want %s", got, tt.wantReqID)
			}

			// The same token in a notification, which carries no id and is
			// dropped rather than answered.
			ntf, nerr := ParseNotification([]byte(withoutIDMember(t, tt.input)))
			if ntf != nil {
				t.Fatalf("a refused notification still produced %+v", ntf)
			}
			if nerr == nil {
				t.Fatal("expected an error")
			}
			if nerr.Code != tt.wantCode {
				t.Fatalf("notification code = %d, want %d", nerr.Code, tt.wantCode)
			}
			if nerr.Message != tt.wantNtf {
				t.Fatalf("notification message = %q, want %q", nerr.Message, tt.wantNtf)
			}
		})
	}
}

// withoutIDMember removes the id member from a message body, turning a request
// into the notification spelling the same peer would send.
func withoutIDMember(t *testing.T, body string) string {
	t.Helper()
	for _, member := range []string{`"id":1,`, `"id":"a",`} {
		if strings.Contains(body, member) {
			return strings.Replace(body, member, "", 1)
		}
	}
	t.Fatalf("body %s carries no id member to drop", body)
	return ""
}

// FuzzParseResponse drives the reply parser over untrusted bytes. A client feeds
// it whatever a server sends, so it must never panic and never hand back a
// response that carries neither outcome or both of them.
func FuzzParseResponse(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
		`{"jsonrpc":"2.0","id":"a","error":{"code":-32601,"message":"nope"}}`,
		`{"jsonrpc":"2.0","result":{}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":null}`,
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"error":"not an object"}`,
		`{"jsonrpc":"2.0","id":1,"error":[1,2]}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
		`{"jsonrpc":2.0,"id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":{},"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}} {"jsonrpc":"2.0","id":2,"result":{}}`,
		`{"jsonrpc":"2.0","id":12345678901234567890,"result":1e10000}`,
		`[]`,
		`{`,
		``,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":\"\\u0000\"}",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		resp, err := ParseResponse(raw)
		if (resp == nil) == (err == nil) {
			t.Fatalf("exactly one of response and error must be set for %q", raw)
		}
		if err != nil {
			if err.Code != CodeParseError && err.Code != CodeInvalidRequest {
				t.Fatalf("unexpected error code %d for %q", err.Code, raw)
			}
			return
		}
		if resp.JSONRPC != Version {
			t.Fatalf("accepted a response with version %q", resp.JSONRPC)
		}
		// Exactly one outcome: a caller branches on which is set, so a reply
		// carrying both or neither must never get through.
		if (len(resp.Result) > 0) == (resp.Error != nil) {
			t.Fatalf("accepted a response with result %q and error %v for %q", resp.Result, resp.Error, raw)
		}
		// The id token is matched against the calls still in flight and logged
		// with them, so an accepted reply always carries one a caller can
		// marshal back out.
		if !json.Valid(resp.ID.Raw()) {
			t.Fatalf("accepted a response whose id token is not JSON: %q (from %q)", resp.ID.Raw(), raw)
		}
	})
}
