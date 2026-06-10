package transport

import (
	"context"
	"sync"
)

// Fake is an in-memory transport for tests, mirroring laravel/mcp's
// FakeTransporter. It records every message sent through Send, and lets a test
// inject inbound messages (driving them through the wired MCP server) without
// any real I/O. mcptest builds its fluent assertions on top of Fake, so its
// recording surface is exported.
//
// Fake is safe for concurrent use. A nil MCPServer is permitted: Inject then
// returns no reply and records nothing, which keeps Fake usable as a pure
// Send-recorder.
type Fake struct {
	srv MCPServer

	mu        sync.Mutex
	sent      [][]byte
	received  [][]byte
	sessionID string
}

// NewFake constructs a Fake transport for srv. srv may be nil to use Fake purely
// as a Send recorder.
func NewFake(srv MCPServer) *Fake {
	return &Fake{srv: srv}
}

// Run is a no-op for the fake transport: there is no stream to loop over. It
// returns immediately with a nil error, honouring an already-cancelled context
// by returning its error so callers that select on Run behave consistently.
func (f *Fake) Run(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// Send records msg as an outbound frame. It always succeeds. The recorded bytes
// are copied so a caller reusing its buffer cannot mutate the recording.
func (f *Fake) Send(ctx context.Context, msg []byte) error {
	cp := make([]byte, len(msg))
	copy(cp, msg)
	f.mu.Lock()
	f.sent = append(f.sent, cp)
	f.mu.Unlock()
	return nil
}

// Inject drives one raw inbound JSON-RPC message through the wired server and
// returns the raw reply (nil when the message is a notification or no server is
// wired). The reply is also recorded as a sent frame, mirroring how a real
// transport would emit it, so mcptest can assert on either Inject's return value
// or the recorded outbound stream. The assigned session id from an initialize
// response is retained for subsequent messages.
func (f *Fake) Inject(ctx context.Context, raw []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cp := make([]byte, len(raw))
	copy(cp, raw)
	f.mu.Lock()
	f.received = append(f.received, cp)
	f.mu.Unlock()

	if f.srv == nil {
		return nil, nil
	}

	res := f.srv.Handle(ctx, raw, f.SessionID())
	if res.SessionID != "" {
		f.setSessionID(res.SessionID)
	}
	if !res.HasResponse || res.Response == nil {
		return nil, nil
	}

	msg, err := encodeResponse(res.Response)
	if err != nil {
		return nil, err
	}
	if err := f.Send(ctx, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// Sent returns a copy of all frames sent through Send (and replies emitted by
// Inject), in order.
func (f *Fake) Sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneFrames(f.sent)
}

// LastSent returns the most recently sent frame, or nil if nothing was sent.
func (f *Fake) LastSent() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	last := f.sent[len(f.sent)-1]
	cp := make([]byte, len(last))
	copy(cp, last)
	return cp
}

// Received returns a copy of all raw inbound frames passed to Inject, in order.
func (f *Fake) Received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneFrames(f.received)
}

// Reset clears all recorded sent and received frames and the session id,
// returning the fake to its initial state for reuse across test cases.
func (f *Fake) Reset() {
	f.mu.Lock()
	f.sent = nil
	f.received = nil
	f.sessionID = ""
	f.mu.Unlock()
}

// SessionID returns the current session id (empty until an initialize response
// assigns one).
func (f *Fake) SessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionID
}

func (f *Fake) setSessionID(id string) {
	f.mu.Lock()
	f.sessionID = id
	f.mu.Unlock()
}

// cloneFrames deep-copies a slice of frames so callers cannot mutate the fake's
// internal recordings.
func cloneFrames(in [][]byte) [][]byte {
	if in == nil {
		return nil
	}
	out := make([][]byte, len(in))
	for i, frame := range in {
		cp := make([]byte, len(frame))
		copy(cp, frame)
		out[i] = cp
	}
	return out
}
