package mcptest

import (
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// This file drives the notification capture against frames a conforming server
// never emits beside its reply. The capture must report them: a frame left out
// of the recording is a notification the negative assertions never see.

// TestNotificationCaptureRefusesAFrameItCannotAttribute asserts the driver
// fails the test when the server emits a frame that is not a well-formed
// notification, instead of leaving it out of the recording where a negative
// assertion would then hold over a notification the server did send. A
// server-to-client request is refused too: the harness has no way to answer
// it, and silently dropping it would hide a tool that waits on a reply the
// test never gives.
func TestNotificationCaptureRefusesAFrameItCannotAttribute(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReason string
	}{
		{"a wrong protocol version", `{"jsonrpc":"1.0","method":"notifications/progress","params":{"progress":1}}`, "not a well-formed JSON-RPC notification"},
		{"no protocol version", `{"method":"notifications/progress","params":{"progress":1}}`, "not a well-formed JSON-RPC notification"},
		{"bytes that are not JSON", `not json at all`, "not a well-formed JSON-RPC notification"},
		{"a method that is not a string", `{"jsonrpc":"2.0","method":5}`, "not a well-formed JSON-RPC notification"},
		{"no method", `{"jsonrpc":"2.0","params":{}}`, "not a well-formed JSON-RPC notification"},
		{"a batch", `[{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}]`, "not a well-formed JSON-RPC notification"},
		{"a server-to-client request", `{"jsonrpc":"2.0","id":9,"method":"sampling/createMessage","params":{}}`, `request "sampling/createMessage" (id 9) that this harness cannot answer`},
		{"a second reply", `{"jsonrpc":"2.0","id":1,"result":{}}`, "not a well-formed JSON-RPC notification"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, drive := range []struct {
				name string
				run  func(ts *Server) *Response
			}{
				{"request", func(ts *Server) *Response { return ts.CallTool("x", nil) }},
				{"notification", func(ts *Server) *Response { return ts.Notify("notifications/initialized", nil) }},
			} {
				stub := stubMCPServer{handle: func(raw []byte, emit func(msg []byte) error) server.HandleResult {
					if emit != nil {
						_ = emit([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`))
						_ = emit([]byte(tc.frame))
					}
					_, id, _ := jsonrpc.ParseRequest(raw)
					if id.IsNull() {
						return server.HandleResult{HasResponse: false}
					}
					resp, _ := jsonrpc.NewResult(id, map[string]any{"content": []any{}, "isError": false})
					return server.HandleResult{Response: resp, HasResponse: true}
				}}
				tb := &recordingTB{}
				res := stubHarness(tb, stub).withNotificationsCheck(drive.run)
				if len(tb.messages) != 1 || !strings.Contains(tb.messages[0], tc.wantReason) {
					t.Fatalf("%s: driver failures = %q, want one naming %q", drive.name, tb.messages, tc.wantReason)
				}
				if !strings.Contains(tb.messages[0], strings.TrimSpace(tc.frame)) {
					t.Fatalf("%s: driver failure %q does not show the frame", drive.name, tb.messages[0])
				}
				// The well-formed frame emitted beside it is still attributed.
				if got := len(res.SentNotifications()); got != 1 {
					t.Fatalf("%s: attributed %d notifications, want the one well-formed frame", drive.name, got)
				}
			}
		})
	}
}

// withNotificationsCheck drives run and returns its reply; it exists so the
// table above reads as one call per row.
func (s *Server) withNotificationsCheck(run func(*Server) *Response) *Response {
	return run(s)
}
