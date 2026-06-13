package client

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// fakeHandler builds the response a fakeTransport returns for a request method.
type fakeHandler func(id jsonrpc.ID, params json.RawMessage) *jsonrpc.Response

// fakeTransport is an in-memory Transport for protocol/client tests. It parses
// each sent frame, invokes the registered handler for the request's method, and
// queues the response for Receive. It can also inject server-initiated frames
// before a response and simulate a one-shot session expiry.
type fakeTransport struct {
	mu       sync.Mutex
	handlers map[string]fakeHandler
	queue    []string
	sent     []string
	connects int

	expireOnce   map[string]bool
	framesBefore map[string][]string
}

var _ Transport = (*fakeTransport)(nil)

// newFakeTransport builds a fakeTransport with a default initialize handler.
func newFakeTransport() *fakeTransport {
	f := &fakeTransport{
		handlers:     map[string]fakeHandler{},
		expireOnce:   map[string]bool{},
		framesBefore: map[string][]string{},
	}
	f.handlers["initialize"] = func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"protocolVersion": server.LatestProtocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake", "version": "1.0.0"},
			"instructions":    "be helpful",
		})
		return resp
	}
	f.handlers["ping"] = func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{})
		return resp
	}
	return f
}

func (f *fakeTransport) on(method string, fn fakeHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = fn
}

func (f *fakeTransport) Connect(context.Context) error {
	f.mu.Lock()
	f.connects++
	f.queue = nil
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Disconnect() error { return nil }

func (f *fakeTransport) SetTimeout(time.Duration) {}

func (f *fakeTransport) Recipe() Recipe { return Recipe{Driver: "fake"} }

func (f *fakeTransport) Send(_ context.Context, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, message)

	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal([]byte(message), &probe)

	if len(probe.ID) == 0 { // notification: no response
		return nil
	}

	if f.expireOnce[probe.Method] {
		f.expireOnce[probe.Method] = false
		f.queue = nil
		return errSessionExpired
	}

	var id jsonrpc.ID
	_ = id.UnmarshalJSON(probe.ID)

	f.queue = append(f.queue, f.framesBefore[probe.Method]...)

	handler, ok := f.handlers[probe.Method]
	if !ok {
		resp := jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeMethodNotFound, "no handler for "+probe.Method)
		f.queue = append(f.queue, marshalResp(resp))
		return nil
	}
	var params json.RawMessage
	if raw := extractParams(message); raw != nil {
		params = raw
	}
	f.queue = append(f.queue, marshalResp(handler(id, params)))
	return nil
}

func (f *fakeTransport) Receive(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return "", newError("fake transport: no message queued")
	}
	msg := f.queue[0]
	f.queue = f.queue[1:]
	return msg, nil
}

func marshalResp(r *jsonrpc.Response) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func extractParams(message string) json.RawMessage {
	var req struct {
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal([]byte(message), &req)
	return req.Params
}
