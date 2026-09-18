package server_test

import (
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// TestParamsShapeOnTheWire asserts a message whose [params] member is not a
// structured value is refused with InvalidParams before any handler sees it,
// and that the refusal carries the id the client can correlate: the request's
// own id for a request, a null id for a notification (which has none).
func TestParamsShapeOnTheWire(t *testing.T) {
	const wantMessage = "Invalid params: The [params] member must be an object."
	tests := []struct {
		name   string
		raw    string
		wantID string
	}{
		{"request with string params", `{"jsonrpc":"2.0","id":1,"method":"ping","params":"nope"}`, "1"},
		{"request with list params", `{"jsonrpc":"2.0","id":"abc","method":"tools/list","params":[1,2]}`, `"abc"`},
		{"request with null params", `{"jsonrpc":"2.0","id":4,"method":"ping","params":null}`, "4"},
		{"request with number params", `{"jsonrpc":"2.0","id":5,"method":"ping","params":7}`, "5"},
		{"notification with list params", `{"jsonrpc":"2.0","method":"notifications/initialized","params":[1]}`, "null"},
		{"notification with string params", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":"x"}`, "null"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			res := handle(t, s, tt.raw)
			if !res.HasResponse || res.Response == nil {
				t.Fatal("a malformed params member must be answered, not dropped")
			}
			err := errorOf(t, res)
			if err.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeInvalidParams)
			}
			if err.Message != wantMessage {
				t.Fatalf("message = %q, want %q", err.Message, wantMessage)
			}
			if got := string(res.Response.ID.Raw()); got != tt.wantID {
				t.Fatalf("id = %s, want %s", got, tt.wantID)
			}
		})
	}
}

// TestTrailingTokenIsAParseError asserts a body carrying anything after its
// first JSON value is refused as malformed rather than parsed as if the extra
// token were not there. A stray closing brace is the interesting case: it ends
// no value the parser is inside, so a permissive reader would accept it and the
// message would then be classified differently by anything that decodes the
// same bytes with the standard library.
func TestTrailingTokenIsAParseError(t *testing.T) {
	tests := []struct{ name, raw string }{
		{"trailing brace", `{"jsonrpc":"2.0","id":1,"method":"ping"}}`},
		{"trailing bracket", `{"jsonrpc":"2.0","id":1,"method":"ping"}]`},
		{"second message", `{"jsonrpc":"2.0","id":1,"method":"ping"} {"jsonrpc":"2.0","id":2,"method":"ping"}`},
		{"trailing garbage", `{"jsonrpc":"2.0","id":1,"method":"ping"} x`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			err := errorOf(t, handle(t, s, tt.raw))
			if err.Code != jsonrpc.CodeParseError {
				t.Fatalf("code = %d, want %d", err.Code, jsonrpc.CodeParseError)
			}
		})
	}
}

// TestTrailingWhitespaceIsAccepted asserts the strictness stops at whitespace:
// a body a client pretty-printed or terminated with a newline is still a valid
// message.
func TestTrailingWhitespaceIsAccepted(t *testing.T) {
	s := server.New("demo", "1.0.0")
	res := handle(t, s, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n\t ")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("want a successful ping, got %+v", res.Response)
	}
}
