package mcptest

import (
	"encoding/json"
	"strings"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file adds the notification-stream assertions. While a request is being
// handled the server may emit server-initiated frames on the same connection
// (notifications/progress and friends, per the MCP progress and logging
// utilities). The Fake transport records those frames alongside the final
// reply, so a driven message is split into one response plus zero or more
// notifications, exactly as a streaming client would observe it.

// sentCount returns the number of frames the Fake transport has recorded so far.
// It is the watermark Server.withNotifications uses to attribute newly emitted
// frames to a single driven message.
func (s *Server) sentCount() int {
	return len(s.fake.Sent())
}

// withNotifications attaches every notification frame the server emitted after
// the sentBefore watermark to r, in emission order, and returns r. Frames that
// carry an id (the final reply) and frames that are not well-formed JSON-RPC
// notifications are ignored.
func (s *Server) withNotifications(sentBefore int, r *Response) *Response {
	frames := s.fake.Sent()
	if sentBefore > len(frames) {
		// The recording was drained while the message was in flight; there is
		// nothing left to attribute to it.
		return r
	}
	for _, frame := range frames[sentBefore:] {
		if note, ok := decodeNotificationFrame(frame); ok {
			r.notifications = append(r.notifications, note)
		}
	}
	return r
}

// decodeNotificationFrame decodes one recorded outbound frame as a JSON-RPC
// notification, reporting false for a reply (a frame with a non-null id) and for
// a frame that is not a notification envelope at all: bytes that are not one
// well-formed JSON object, a jsonrpc member other than "2.0", or a missing or
// non-string method.
//
// The params member is carried through exactly as the server wrote it, whatever
// its shape. The inbound parser is deliberately stricter and rejects params that
// are not an object, the specification modelling them as one, but that is a
// judgement about what a server must accept, not about what it emitted. Reusing
// it here would silently drop such a frame from the recording, leaving
// AssertNotificationCount(0) and AssertNotSentNotification green over a
// notification that was in fact emitted: a server defect read as a clean test.
// So every frame that is recognisably a notification is attributed, and its
// params are left for the assertions to render and compare as they arrived.
func decodeNotificationFrame(frame []byte) (*jsonrpc.Notification, bool) {
	// IsNotificationBytes settles the two questions the envelope alone answers:
	// the bytes hold exactly one JSON object (nothing trailing it), and it
	// carries no usable id, so it is not the final reply.
	isNotification, err := jsonrpc.IsNotificationBytes(frame)
	if err != nil || !isNotification {
		return nil, false
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(frame, &members); err != nil {
		return nil, false
	}

	var version string
	if err := json.Unmarshal(members["jsonrpc"], &version); err != nil || version != jsonrpc.Version {
		return nil, false
	}

	// The method is decoded as a free-form value and then required to be a
	// string: decoding straight into a string would report success for a JSON
	// null, which encoding/json leaves the destination untouched for, and the
	// frame would pass as one naming the empty method.
	var methodValue any
	if err := json.Unmarshal(members["method"], &methodValue); err != nil {
		return nil, false
	}
	method, isString := methodValue.(string)
	if !isString {
		return nil, false
	}

	note := &jsonrpc.Notification{JSONRPC: jsonrpc.Version, Method: method}
	if params, present := members["params"]; present {
		note.Params = append(json.RawMessage(nil), params...)
	}
	return note, true
}

// SentNotifications returns the notifications the server emitted while handling
// this message, in emission order.
//
// The caller owns the result outright: the slice, every notification in it and
// every params buffer are copies, so writing into any of them disturbs neither
// the recording nor another reader. Copying the slice alone would not be enough,
// the recorded notifications being pointers into shared state: one caller
// retitling a notification would change what the assertions and every other
// caller then see, and two callers writing at once would race.
func (r *Response) SentNotifications() []*jsonrpc.Notification {
	out := make([]*jsonrpc.Notification, len(r.notifications))
	for i, note := range r.notifications {
		out[i] = cloneNotification(note)
	}
	return out
}

// cloneNotification copies a recorded notification, params buffer included. A
// nil notification is never recorded, but is carried through rather than
// dereferenced.
func cloneNotification(note *jsonrpc.Notification) *jsonrpc.Notification {
	if note == nil {
		return nil
	}
	clone := *note
	if note.Params != nil {
		clone.Params = append(json.RawMessage(nil), note.Params...)
	}
	return &clone
}

// AssertNotificationCount asserts the server emitted exactly n notifications
// while handling this message.
func (r *Response) AssertNotificationCount(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if got := len(r.notifications); got != n {
		r.fatalf("mcptest: %s: emitted %d notifications, want %d; emitted: %s",
			r.method, got, n, describeMethods(r.notificationMethods()))
	}
	return r
}

// AssertSentNotification asserts the server emitted at least one notification
// with the given method while handling this message. Use
// AssertSentNotificationWith to also pin the notification params.
func (r *Response) AssertSentNotification(method string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	for _, note := range r.notifications {
		if note.Method == method {
			return r
		}
	}
	r.fatalf("mcptest: %s: expected a %q notification, but none was emitted; emitted: %s",
		r.method, method, describeMethods(r.notificationMethods()))
	return r
}

// AssertSentNotificationWith asserts the server emitted a notification with the
// given method whose params equal want, whole: a partial expectation does not
// match. Both sides are compared as their serialized forms, so an int literal
// matches the number on the wire. A notification emitted without params matches
// an empty want, omitting the optional member and sending {} being the same
// statement on the wire; params written as an explicit null are not, the
// specification modelling them as an object, and do not match it. A nil want
// matches any params, making it equivalent to AssertSentNotification.
func (r *Response) AssertSentNotificationWith(method string, want map[string]any) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if want == nil {
		return r.AssertSentNotification(method)
	}
	var seen []string
	for _, note := range r.notifications {
		if note.Method != method {
			continue
		}
		if jsonEqual(notificationParams(note), want) {
			return r
		}
		seen = append(seen, describeNotificationParams(note))
	}
	if len(seen) == 0 {
		r.fatalf("mcptest: %s: expected a %q notification with params %s, but no %q notification was emitted; emitted: %s",
			r.method, method, jsonString(want), method, describeMethods(r.notificationMethods()))
		return r
	}
	r.fatalf("mcptest: %s: expected a %q notification with params %s, got: %s",
		r.method, method, jsonString(want), strings.Join(seen, ", "))
	return r
}

// AssertNotSentNotification asserts the server emitted no notification with the
// given method while handling this message.
func (r *Response) AssertNotSentNotification(method string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	for _, note := range r.notifications {
		if note.Method == method {
			r.fatalf("mcptest: %s: did not expect a %q notification, but one was emitted with params %s",
				r.method, method, describeNotificationParams(note))
			return r
		}
	}
	return r
}

// notificationParams returns a notification's params as the server wrote them,
// with omitted params standing in as an empty object: the params member is
// optional, and leaving it out makes the same statement as sending {}, so an
// empty expectation must match both.
//
// Nothing else is normalised. The specification models params as an object, so
// an explicit null, a scalar, an array and malformed bytes are all output no
// server may emit; folding them into {} would let a positive assertion hold over
// a frame the specification does not allow, hiding the defect that produced it.
// They are carried through instead and simply fail to equal an empty
// expectation.
func notificationParams(note *jsonrpc.Notification) json.RawMessage {
	if len(note.Params) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(note.Params)
}

// describeNotificationParams renders a notification's params for a failure
// message from the bytes that arrived, so params of any shape (an array, a
// scalar, none at all) are reported as they were sent.
func describeNotificationParams(note *jsonrpc.Notification) string {
	return describeJSON(json.RawMessage(note.Params))
}

// notificationMethods returns the method of every emitted notification, in
// order and with duplicates kept (the count is part of what a failure reports).
func (r *Response) notificationMethods() []string {
	out := make([]string, 0, len(r.notifications))
	for _, note := range r.notifications {
		out = append(out, note.Method)
	}
	return out
}

// describeMethods renders a method list for a failure message, naming the empty
// case explicitly rather than printing an empty string.
func describeMethods(methods []string) string {
	if len(methods) == 0 {
		return "(none)"
	}
	return strings.Join(methods, ", ")
}
