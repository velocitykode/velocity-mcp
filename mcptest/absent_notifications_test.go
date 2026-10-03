package mcptest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// This file drives the two assertions that say a notification is absent
// against replies that do not show the message was carried out. "Nothing was
// emitted" said of a call that was refused, or of a message nothing answered,
// holds whether or not the handler exists, so a test leaning on it is green
// over a tool that never ran.

// absentNotificationAssertions are the assertions under test, each stating
// that no notification was emitted.
var absentNotificationAssertions = []struct {
	name string
	fn   func(*Response)
}{
	{"AssertNotSentNotification", func(r *Response) { r.AssertNotSentNotification("notifications/progress") }},
	{"AssertNotificationCount(0)", func(r *Response) { r.AssertNotificationCount(0) }},
}

func TestAbsentNotificationAssertionsNeedAReplyThatShowsTheWorkWasDone(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		// wantReason is part of the failure message; empty means the
		// assertions hold on this reply.
		wantReason string
	}{
		{name: "no reply was produced", frame: ``, wantReason: "no reply was produced"},
		{name: "a protocol error", frame: `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Tool [typo] not found."}}`, wantReason: "protocol error -32602: Tool [typo] not found."},
		{name: "neither result nor error", frame: `{"jsonrpc":"2.0","id":1}`, wantReason: "neither a result nor an error"},
		{name: "a result", frame: okFrame(`{"content":[],"isError":false}`)},
		{name: "an empty result", frame: okFrame(`{}`)},
		// A tool that failed did run, and what it emitted on the way is a fair
		// thing to assert about.
		{name: "a tool-level failure", frame: okFrame(`{"content":[{"type":"text","text":"bad"}],"isError":true}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, assertion := range absentNotificationAssertions {
				r := replyFrom(t, nil, "tools/call", tc.frame)
				if tc.wantReason == "" {
					assertHolds(t, r, assertion.fn)
					continue
				}
				message := assertFails(t, r, assertion.fn)
				if !strings.Contains(message, tc.wantReason) {
					t.Fatalf("%s failure message = %q, want it to say %q", assertion.name, message, tc.wantReason)
				}
				if !strings.Contains(message, "SentNotifications") {
					t.Fatalf("%s failure message = %q, want it to name SentNotifications as the way to read the recording", assertion.name, message)
				}
			}
		})
	}
}

// What was recorded is reported as it always was, whatever the reply: a
// notification that was emitted fails the "not sent" assertion by name, and a
// count is compared against the frames recorded. Only an absence is held to
// the reply.
func TestNotificationAssertionsStillReadTheRecordingOnAnyReply(t *testing.T) {
	const errorFrame = `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`
	progress := &jsonrpc.Notification{JSONRPC: jsonrpc.Version, Method: "notifications/progress", Params: []byte(`{"progress":1}`)}
	logged := &jsonrpc.Notification{JSONRPC: jsonrpc.Version, Method: "notifications/message"}

	cases := []struct {
		name     string
		frame    string
		recorded []*jsonrpc.Notification
		assert   func(*Response)
		// wantReason is part of the failure message; empty means the
		// assertion holds.
		wantReason string
	}{
		{name: "the emitted notification is named on an error reply", frame: errorFrame, recorded: []*jsonrpc.Notification{progress},
			assert: func(r *Response) { r.AssertNotSentNotification("notifications/progress") }, wantReason: `one was emitted with params {"progress":1}`},
		{name: "the emitted notification is named on no reply", frame: ``, recorded: []*jsonrpc.Notification{progress},
			assert: func(r *Response) { r.AssertNotSentNotification("notifications/progress") }, wantReason: `one was emitted with params {"progress":1}`},
		{name: "another method was emitted on an error reply", frame: errorFrame, recorded: []*jsonrpc.Notification{logged},
			assert: func(r *Response) { r.AssertNotSentNotification("notifications/progress") }, wantReason: "protocol error -32603: boom"},
		{name: "a zero count over a recorded frame reports the count", frame: errorFrame, recorded: []*jsonrpc.Notification{progress},
			assert: func(r *Response) { r.AssertNotificationCount(0) }, wantReason: "emitted 1 notifications, want 0"},
		{name: "a positive count holds on an error reply", frame: errorFrame, recorded: []*jsonrpc.Notification{progress, logged},
			assert: func(r *Response) { r.AssertNotificationCount(2) }},
		{name: "a positive count holds on no reply", frame: ``, recorded: []*jsonrpc.Notification{progress},
			assert: func(r *Response) { r.AssertNotificationCount(1) }},
		{name: "a positive count that is wrong reports the count", frame: errorFrame,
			assert: func(r *Response) { r.AssertNotificationCount(1) }, wantReason: "emitted 0 notifications, want 1"},
		{name: "a sent assertion holds on an error reply", frame: errorFrame, recorded: []*jsonrpc.Notification{progress},
			assert: func(r *Response) { r.AssertSentNotification("notifications/progress") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := replyFrom(t, nil, "tools/call", tc.frame)
			r.notifications = tc.recorded
			if tc.wantReason == "" {
				assertHolds(t, r, tc.assert)
				return
			}
			if message := assertFails(t, r, tc.assert); !strings.Contains(message, tc.wantReason) {
				t.Fatalf("failure message = %q, want it to say %q", message, tc.wantReason)
			}
		})
	}
}

// The same rule seen from the harness: a call the server refuses and a
// notification, which is never answered, both leave the absence assertions
// with nothing to stand on, while a call that was answered with a result
// supports them. The recording itself stays readable for all three.
func TestAbsentNotificationAssertionsThroughTheHarness(t *testing.T) {
	stub := stubMCPServer{handle: func(raw []byte, _ func(msg []byte) error) server.HandleResult {
		req, id, _ := jsonrpc.ParseRequest(raw)
		if id.IsNull() {
			return server.HandleResult{HasResponse: false}
		}
		var params struct {
			Name string `json:"name"`
		}
		if req != nil {
			_ = json.Unmarshal(req.Params, &params)
		}
		if params.Name != "known" {
			resp := jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeInvalidParams, "Tool ["+params.Name+"] not found.")
			return server.HandleResult{Response: resp, HasResponse: true}
		}
		resp, _ := jsonrpc.NewResult(id, map[string]any{"content": []any{}, "isError": false})
		return server.HandleResult{Response: resp, HasResponse: true}
	}}

	cases := []struct {
		name string
		run  func(ts *Server) *Response
		// wantReason is part of each failure message; empty means the
		// assertions hold.
		wantReason string
	}{
		{name: "a call to a tool that is not there", run: func(ts *Server) *Response { return ts.CallTool("no-such-tool", nil) }, wantReason: "protocol error -32602: Tool [no-such-tool] not found."},
		{name: "a notification", run: func(ts *Server) *Response { return ts.Notify("notifications/initialized", nil) }, wantReason: "no reply was produced"},
		{name: "a call that was answered", run: func(ts *Server) *Response { return ts.CallTool("known", nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, assertion := range absentNotificationAssertions {
				tb := &recordingTB{}
				r := tc.run(stubHarness(tb, stub))
				if len(tb.messages) != 0 {
					t.Fatalf("driving the message failed: %v", tb.messages)
				}
				if got := r.SentNotifications(); len(got) != 0 {
					t.Fatalf("SentNotifications = %v, want none", got)
				}
				assertion.fn(r)
				switch {
				case tc.wantReason == "" && len(tb.messages) != 0:
					t.Fatalf("%s failed on a call that was answered: %v", assertion.name, tb.messages)
				case tc.wantReason != "" && (len(tb.messages) != 1 || !strings.Contains(tb.messages[0], tc.wantReason)):
					t.Fatalf("%s failures = %v, want exactly one saying %q", assertion.name, tb.messages, tc.wantReason)
				}
			}
		})
	}
}
