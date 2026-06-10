package jsonrpc

import (
	"encoding/json"
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
	// correlate the error response, mirroring laravel.
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
		// A present-but-null id is routed as a notification (no reply), mirroring
		// laravel/mcp's isset()-based routing in Server::handle: isset() is false
		// for a present-but-null key, so {"id":null,...} becomes a notification.
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
