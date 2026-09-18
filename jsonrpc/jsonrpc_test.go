package jsonrpc

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestErrorCodes(t *testing.T) {
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"ParseError", CodeParseError, -32700},
		{"InvalidRequest", CodeInvalidRequest, -32600},
		{"MethodNotFound", CodeMethodNotFound, -32601},
		{"InvalidParams", CodeInvalidParams, -32602},
		{"InternalError", CodeInternalError, -32603},
		{"ResourceNotFound", CodeResourceNotFound, -32002},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("code = %d, want %d", tt.got, tt.want)
			}
		})
	}
	if Version != "2.0" {
		t.Fatalf("Version = %q, want 2.0", Version)
	}
}

func TestID_Constructors(t *testing.T) {
	tests := []struct {
		name      string
		id        ID
		wantJSON  string
		wantStr   string
		wantNull  bool
		wantValid bool
	}{
		{"string", StringID("abc"), `"abc"`, "abc", false, true},
		{"empty string", StringID(""), `""`, "", false, true},
		{"int", IntID(42), `42`, "42", false, true},
		{"negative int", IntID(-7), `-7`, "-7", false, true},
		{"zero int", IntID(0), `0`, "0", false, true},
		{"null", NullID(), `null`, "", true, false},
		{"zero value", ID{}, `null`, "", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.id)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.wantJSON {
				t.Errorf("json = %s, want %s", b, tt.wantJSON)
			}
			if got := tt.id.String(); got != tt.wantStr {
				t.Errorf("String() = %q, want %q", got, tt.wantStr)
			}
			if got := tt.id.IsNull(); got != tt.wantNull {
				t.Errorf("IsNull() = %v, want %v", got, tt.wantNull)
			}
			if got := tt.id.IsValidRequestID(); got != tt.wantValid {
				t.Errorf("IsValidRequestID() = %v, want %v", got, tt.wantValid)
			}
		})
	}
}

func TestID_RoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantStr string
	}{
		{"string", `"hello"`, "hello"},
		{"int", `123`, "123"},
		{"float", `1.5`, "1.5"},
		{"null", `null`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var id ID
			if err := json.Unmarshal([]byte(tt.raw), &id); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := id.String(); got != tt.wantStr {
				t.Errorf("String() = %q, want %q", got, tt.wantStr)
			}
			// Marshaling preserves the original token.
			b, err := json.Marshal(id)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.raw {
				t.Errorf("re-marshal = %s, want %s", b, tt.raw)
			}
		})
	}
}

func TestID_IsValidRequestID(t *testing.T) {
	// Cases built from raw tokens directly so we can exercise the numeric-prefix
	// branches (+/./-) that the JSON grammar never actually produces.
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"string", `"x"`, true},
		{"positive int", `5`, true},
		{"negative", `-5`, true},
		{"float", `1.2`, true},
		{"leading plus", `+5`, true},
		{"leading dot", `.5`, true},
		{"null", `null`, false},
		{"bool true", `true`, false},
		{"object", `{}`, false},
		{"array", `[]`, false},
		{"empty token", ``, false},
		{"whitespace only", `  `, false},
		{"leading whitespace number", ` 9 `, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := ID{raw: json.RawMessage(tt.raw)}
			if got := id.IsValidRequestID(); got != tt.want {
				t.Errorf("IsValidRequestID(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestID_Raw(t *testing.T) {
	id := IntID(7)
	raw := id.Raw()
	if string(raw) != "7" {
		t.Fatalf("Raw() = %s, want 7", raw)
	}
	// Mutating the returned copy must not affect the id.
	raw[0] = '9'
	if id.String() != "7" {
		t.Fatalf("Raw() returned aliased slice; id mutated to %q", id.String())
	}
	// Zero value yields nil.
	if (ID{}).Raw() != nil {
		t.Fatalf("zero ID Raw() should be nil")
	}
}

func TestError(t *testing.T) {
	e := NewError(CodeInvalidParams, "bad params")
	if e.Code != CodeInvalidParams || e.Message != "bad params" || e.Data != nil {
		t.Fatalf("unexpected error: %+v", e)
	}
	if e.Error() != "jsonrpc: code -32602: bad params" {
		t.Fatalf("Error() = %q", e.Error())
	}
	// errors.As interop.
	var target *Error
	if !errors.As(error(e), &target) {
		t.Fatalf("errors.As failed")
	}
	// nil receiver is safe.
	var nilErr *Error
	if nilErr.Error() != "<nil jsonrpc error>" {
		t.Fatalf("nil Error() = %q", nilErr.Error())
	}
	if nilErr.WithData("x") != nil {
		t.Fatalf("nil WithData should return nil")
	}
}

func TestError_WithData(t *testing.T) {
	base := NewError(CodeInternalError, "boom")
	withData := base.WithData(map[string]any{"k": "v"})
	if base.Data != nil {
		t.Fatalf("WithData mutated original")
	}
	if withData.Data == nil {
		t.Fatalf("WithData did not set data")
	}
	b, err := json.Marshal(withData)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"code":-32603,"message":"boom","data":{"k":"v"}}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
	// Without data, the data field is omitted.
	b2, _ := json.Marshal(base)
	if string(b2) != `{"code":-32603,"message":"boom"}` {
		t.Fatalf("omitempty failed: %s", b2)
	}
}

func TestNewResult(t *testing.T) {
	tests := []struct {
		name   string
		id     ID
		result any
		want   string
	}{
		{"nil result -> empty object", IntID(1), nil, `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"map result", StringID("a"), map[string]any{"ok": true}, `{"jsonrpc":"2.0","id":"a","result":{"ok":true}}`},
		{"raw message", IntID(2), json.RawMessage(`[1,2]`), `{"jsonrpc":"2.0","id":2,"result":[1,2]}`},
		{"empty raw -> empty object", IntID(3), json.RawMessage(`  `), `{"jsonrpc":"2.0","id":3,"result":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := NewResult(tt.id, tt.result)
			if err != nil {
				t.Fatalf("NewResult: %v", err)
			}
			b, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("json = %s, want %s", b, tt.want)
			}
		})
	}
}

func TestNewResult_MarshalError(t *testing.T) {
	_, err := NewResult(IntID(1), make(chan int))
	if err == nil {
		t.Fatalf("expected marshal error for unmarshalable result")
	}
}

func TestNewResult_RawNotAliased(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	resp, err := NewResult(IntID(1), raw)
	if err != nil {
		t.Fatalf("NewResult: %v", err)
	}
	resp.Result[0] = 'X'
	if string(raw) != `{"a":1}` {
		t.Fatalf("input raw was aliased and mutated: %s", raw)
	}
}

func TestNewErrorResponse(t *testing.T) {
	resp := NewErrorResponse(IntID(9), NewError(CodeMethodNotFound, "nope"))
	b, _ := json.Marshal(resp)
	want := `{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"nope"}}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}

	resp2 := NewErrorResponseCode(NullID(), CodeParseError, msgParseError)
	b2, _ := json.Marshal(resp2)
	want2 := `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"` + msgParseError + `"}}`
	if string(b2) != want2 {
		t.Fatalf("json = %s, want %s", b2, want2)
	}
}

func TestNewNotification(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params any
		want   string
	}{
		{"with params", "notifications/message", map[string]any{"level": "info"}, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`},
		{"nil params omitted", "notifications/initialized", nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewNotification(tt.method, tt.params)
			if err != nil {
				t.Fatalf("NewNotification: %v", err)
			}
			b, _ := json.Marshal(n)
			if string(b) != tt.want {
				t.Fatalf("json = %s, want %s", b, tt.want)
			}
		})
	}
}

func TestNewNotification_MarshalError(t *testing.T) {
	if _, err := NewNotification("x", make(chan int)); err == nil {
		t.Fatalf("expected marshal error")
	}
}

// TestEncodeNotification asserts the one-pass encoder writes the same frame the
// two-step form does: a JSON-RPC 2.0 notification object carrying the version,
// the method and the params, with no id, and with an absent params member when
// there are none.
func TestEncodeNotification(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params any
		want   string
	}{
		{"with params", "notifications/message", map[string]any{"level": "info"}, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`},
		{"nil params omitted", "notifications/initialized", nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		{"empty params object is stated", "notifications/progress", map[string]any{}, `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`},
		{"a raw params token travels verbatim", "notifications/message", json.RawMessage(`{"level":"warn"}`), `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"warn"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := EncodeNotification(tt.method, tt.params)
			if err != nil {
				t.Fatalf("EncodeNotification: %v", err)
			}
			if string(frame) != tt.want {
				t.Fatalf("frame = %s, want %s", frame, tt.want)
			}
		})
	}
}

// TestEncodeNotificationRefusesParamsItCannotWrite asserts params that cannot
// be encoded are reported instead of yielding a half-written frame.
func TestEncodeNotificationRefusesParamsItCannotWrite(t *testing.T) {
	for _, params := range []any{make(chan int), map[string]any{"fn": func() {}}, math.Inf(-1)} {
		frame, err := EncodeNotification("notifications/message", params)
		if err == nil {
			t.Fatalf("params %T were accepted as %s", params, frame)
		}
		if frame != nil {
			t.Fatalf("a refused notification still produced %s", frame)
		}
	}
}

// TestIDRefusesATokenThatIsNotJSON asserts an id read straight off untrusted
// bytes is refused there rather than carried until something tries to write it
// back out. A transport recovers the id of a message it has not parsed this
// way, so these are the bytes a peer controls.
func TestIDRefusesATokenThatIsNotJSON(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ``},
		{"an unclosed object", `{`},
		{"an unterminated string", `"abc`},
		{"a bare word", `nope`},
		{"a number with a leading zero", `01`},
		{"two values", `1 2`},
		{"a lone NUL byte", "\x00"},
		{"an unpaired surrogate escape", `"\ud800`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := StringID("previous")
			if err := id.UnmarshalJSON([]byte(tt.token)); err == nil {
				t.Fatalf("token %q was accepted as %s", tt.token, id.Raw())
			}
			// The refused token leaves no id behind: a reply carrying it would
			// otherwise correlate to the call the last one named.
			if !id.IsNull() {
				t.Fatalf("a refused token left the id %s", id.Raw())
			}
			if id.IsValidRequestID() {
				t.Fatalf("a refused token left a usable request id %s", id.Raw())
			}
			b, err := json.Marshal(id)
			if err != nil {
				t.Fatalf("marshal a refused id: %v", err)
			}
			if string(b) != "null" {
				t.Fatalf("json = %s, want null", b)
			}
		})
	}
}

// TestIDKeepsAnAcceptedTokenVerbatim asserts the other half: a token that is
// JSON is preserved byte for byte, including a numeric spelling Go would not
// have written, and can always be written back out.
func TestIDKeepsAnAcceptedTokenVerbatim(t *testing.T) {
	for _, token := range []string{`1`, `1.50`, `12345678901234567890`, `"a"`, `""`, `null`, `{"not":"a request id"}`} {
		t.Run(token, func(t *testing.T) {
			var id ID
			if err := id.UnmarshalJSON([]byte(token)); err != nil {
				t.Fatalf("token %q refused: %v", token, err)
			}
			if got := string(id.Raw()); got != token {
				t.Fatalf("raw = %s, want %s", got, token)
			}
			b, err := json.Marshal(id)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != token {
				t.Fatalf("json = %s, want %s", b, token)
			}
		})
	}
}
