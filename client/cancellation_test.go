package client

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// This file covers the decision the protocol makes when an exchange is
// abandoned, over transports that answer the CancellationAware hook each way
// and over one that does not answer it at all.

// stallingTransport answers the handshake of the era it is given and stalls on
// every other request until the context of the read is done, which it reports
// the way the transports of this package do. It records the frames it was sent
// and how often it was torn down.
type stallingTransport struct {
	legacy bool
	// stallHandshake makes the handshake itself the request that stalls.
	stallHandshake bool
	// refuseNotice makes the transport fail the sending of a notification.
	refuseNotice bool

	mu          sync.Mutex
	queue       []string
	sent        []string
	disconnects int
}

var _ Transport = (*stallingTransport)(nil)

func (s *stallingTransport) Connect(context.Context) error { return nil }
func (s *stallingTransport) SetTimeout(time.Duration)      {}
func (s *stallingTransport) Recipe() Recipe                { return Recipe{Driver: "stalling"} }

func (s *stallingTransport) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnects++
	return nil
}

func (s *stallingTransport) Send(_ context.Context, message string) error {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal([]byte(message), &frame)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(frame.ID) == 0 && s.refuseNotice && frame.Method == "notifications/cancelled" {
		return NewTransportError("the channel is gone", nil)
	}
	s.sent = append(s.sent, message)
	switch {
	case s.stallHandshake:
	case frame.Method == "server/discover" && !s.legacy:
		s.queue = append(s.queue, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"resultType":"complete",`+
			`"supportedVersions":["`+LatestProtocolVersion+`"],"capabilities":{},"ttlMs":600000}}`)
	case frame.Method == "server/discover":
		s.queue = append(s.queue, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"error":{"code":-32601,"message":"Method not found."}}`)
	case frame.Method == "initialize":
		s.queue = append(s.queue, `{"jsonrpc":"2.0","id":`+string(frame.ID)+`,"result":{"protocolVersion":"`+ProtocolV20251125+
			`","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`)
	}
	return nil
}

func (s *stallingTransport) Receive(ctx context.Context) (string, error) {
	s.mu.Lock()
	if len(s.queue) > 0 {
		frame := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		return frame, nil
	}
	s.mu.Unlock()

	<-ctx.Done()
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", wrapError(ctx.Err(), "the wait was cancelled")
	}
	return "", NewTimeoutError("timed out", ctx.Err())
}

// frames returns the frames sent so far, decoded far enough to assert on.
func (s *stallingTransport) frames(t *testing.T) []loggedFrame {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	frames := make([]loggedFrame, 0, len(s.sent))
	for _, raw := range s.sent {
		var frame loggedFrame
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("frame %q: %v", raw, err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func (s *stallingTransport) tornDown() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disconnects
}

// notifyingTransport is a stalling transport that answers the hook, with the
// answer it is given.
type notifyingTransport struct {
	*stallingTransport
	notifies bool
}

var _ CancellationAware = notifyingTransport{}

func (n notifyingTransport) NotifiesCancellation(ProtocolVersion) bool { return n.notifies }

// TestAnAbandonedExchangeIsWithdrawnAsTheTransportSays walks the decision: who
// is told, what it costs, and that a handshake is never the request cancelled.
func TestAnAbandonedExchangeIsWithdrawnAsTheTransportSays(t *testing.T) {
	deadline := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 50*time.Millisecond)
	}
	withdrawn := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		return ctx, cancel
	}

	tests := []struct {
		name string
		// transport wraps the stalling transport as the case needs.
		transport func(*stallingTransport) Transport
		setup     func(*stallingTransport)
		ctx       func() (context.Context, context.CancelFunc)
		// wantNotice is whether a notifications/cancelled frame goes out, and
		// wantMeta whether it carries the protocol metadata.
		wantNotice bool
		wantMeta   bool
		wantReason string
		wantStands bool
	}{
		{
			name:       "a transport that asks for the notification is sent one",
			transport:  func(s *stallingTransport) Transport { return notifyingTransport{s, true} },
			ctx:        withdrawn,
			wantNotice: true, wantMeta: true, wantReason: "the caller withdrew the request", wantStands: true,
		},
		{
			name:       "a timeout is withdrawn the same way",
			transport:  func(s *stallingTransport) Transport { return notifyingTransport{s, true} },
			ctx:        deadline,
			wantNotice: true, wantMeta: true, wantReason: "the request timed out", wantStands: true,
		},
		{
			name:       "an initialize-era notification carries no protocol metadata",
			transport:  func(s *stallingTransport) Transport { return notifyingTransport{s, true} },
			setup:      func(s *stallingTransport) { s.legacy = true },
			ctx:        withdrawn,
			wantNotice: true, wantReason: "the caller withdrew the request", wantStands: true,
		},
		{
			name:       "a transport that withdrew the request itself is sent nothing",
			transport:  func(s *stallingTransport) Transport { return notifyingTransport{s, false} },
			ctx:        withdrawn,
			wantStands: true,
		},
		{
			name:      "a transport without the hook loses the connection, as before",
			transport: func(s *stallingTransport) Transport { return s },
			ctx:       withdrawn,
		},
		{
			name:      "a cancellation that cannot be sent is the channel failing",
			transport: func(s *stallingTransport) Transport { return notifyingTransport{s, true} },
			setup:     func(s *stallingTransport) { s.refuseNotice = true },
			ctx:       deadline,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stalling := &stallingTransport{}
			if tt.setup != nil {
				tt.setup(stalling)
			}
			c := New(tt.transport(stalling), testClientInfo())
			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			ctx, cancel := tt.ctx()
			defer cancel()
			if _, err := c.Prompts(ctx); err == nil {
				t.Fatal("a request that was never answered reported success")
			}

			frames := stalling.frames(t)
			abandoned := framesOf(frames, "prompts/list")
			notices := framesOf(frames, "notifications/cancelled")
			if len(abandoned) != 1 {
				t.Fatalf("%d prompts/list frame(s) were sent, want 1", len(abandoned))
			}
			if got := len(notices) == 1; got != tt.wantNotice || len(notices) > 1 {
				t.Fatalf("%d cancellation(s) were sent, want one = %v", len(notices), tt.wantNotice)
			}
			if tt.wantNotice {
				notice := notices[0]
				if string(notice.Params.RequestID) != string(abandoned[0].ID) {
					t.Fatalf("the cancellation names request %s, want %s", notice.Params.RequestID, abandoned[0].ID)
				}
				if notice.Params.Reason != tt.wantReason {
					t.Fatalf("reason = %q, want %q", notice.Params.Reason, tt.wantReason)
				}
				if got := notice.Params.Meta != nil; got != tt.wantMeta {
					t.Fatalf("protocol metadata present = %v, want %v", got, tt.wantMeta)
				}
			}
			if got := c.Connected(); got != tt.wantStands {
				t.Fatalf("connected = %v, want %v", got, tt.wantStands)
			}
			if got := stalling.tornDown() == 0; got != tt.wantStands {
				t.Fatalf("the transport was torn down %d time(s), want none = %v", stalling.tornDown(), tt.wantStands)
			}
		})
	}
}

// TestAHandshakeRequestIsNeverCancelled asserts a handshake that is abandoned
// sends no cancellation: the specification has no client cancel the initialize
// request, and no revision is settled yet to say what one would mean.
func TestAHandshakeRequestIsNeverCancelled(t *testing.T) {
	stalling := &stallingTransport{stallHandshake: true}
	c := New(notifyingTransport{stalling, true}, testClientInfo())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Connect(ctx); err == nil {
		t.Fatal("a handshake that was never answered reported success")
	}
	if notices := framesOf(stalling.frames(t), "notifications/cancelled"); len(notices) != 0 {
		t.Fatalf("%d cancellation(s) were sent for a handshake request", len(notices))
	}
	if c.Connected() {
		t.Fatal("a handshake that was abandoned left a connection standing")
	}
}
