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
	headers  []map[string]string
	versions []ProtocolVersion
	connects int

	expireOnce   map[string]bool
	framesBefore map[string][]string
}

var (
	_ Transport     = (*fakeTransport)(nil)
	_ ProtocolAware = (*fakeTransport)(nil)
	_ HeaderSender  = (*fakeTransport)(nil)
)

// newFakeTransport builds a fakeTransport with a default initialize handler.
func newFakeTransport() *fakeTransport {
	f := &fakeTransport{
		handlers:     map[string]fakeHandler{},
		expireOnce:   map[string]bool{},
		framesBefore: map[string][]string{},
	}
	f.handlers["initialize"] = func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			// The initialize handshake negotiates only over the legacy
			// revisions; the latest revision opens with server/discover.
			"protocolVersion": server.InitializeSupportedVersions()[0],
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

// without drops a method from the server, which then rejects it the way a
// server that does not implement it does: with a method-not-found.
func (f *fakeTransport) without(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.handlers, method)
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

// UseProtocol records the protocol version of each frame the client sends.
func (f *fakeTransport) UseProtocol(version ProtocolVersion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions = append(f.versions, version)
}

// SendWithHeaders records the protocol headers alongside the frame.
func (f *fakeTransport) SendWithHeaders(ctx context.Context, message string, headers map[string]string) error {
	f.mu.Lock()
	f.headers = append(f.headers, headers)
	f.mu.Unlock()
	return f.Send(ctx, message)
}

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

// sentMethods returns the JSON-RPC method of each frame the client sent, in
// order.
func sentMethods(f *fakeTransport) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	methods := make([]string, 0, len(f.sent))
	for _, frame := range f.sent {
		var probe struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal([]byte(frame), &probe)
		methods = append(methods, probe.Method)
	}
	return methods
}

func extractParams(message string) json.RawMessage {
	var req struct {
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal([]byte(message), &req)
	return req.Params
}
