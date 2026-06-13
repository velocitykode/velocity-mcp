package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
)

// protocol drives the JSON-RPC exchange over a transport: it owns request-id
// allocation, the connect/initialize handshake, and the send-then-receive loop
// (including answering server-initiated pings). It is the client's internal
// engine; Client and WebClient are the public surface.
type protocol struct {
	transport  Transport
	clientInfo schema.Implementation

	mu         sync.Mutex
	connected  bool
	connecting bool
	nextID     int64
	initResult *InitializeResult
}

// newProtocol builds a protocol over the given transport and client identity.
func newProtocol(transport Transport, clientInfo schema.Implementation) *protocol {
	return &protocol{transport: transport, clientInfo: clientInfo, nextID: 1}
}

// connected reports whether the initialize handshake has completed.
func (p *protocol) isConnected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connected
}

// initializeResult returns the server's initialize result, or nil before connect.
func (p *protocol) initializeResult() *InitializeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initResult
}

// connect opens the transport and performs the initialize handshake followed by
// the notifications/initialized notification. It is idempotent.
func (p *protocol) connect(ctx context.Context) error {
	p.mu.Lock()
	if p.connected {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	if err := p.transport.Connect(ctx); err != nil {
		return err
	}

	p.mu.Lock()
	p.connecting = true
	p.mu.Unlock()

	result, err := p.initialize(ctx)
	if err == nil {
		err = p.notify(ctx, "notifications/initialized")
	}

	p.mu.Lock()
	p.connecting = false
	if err != nil {
		p.mu.Unlock()
		p.disconnect()
		return err
	}
	p.initResult = result
	p.connected = true
	p.mu.Unlock()
	return nil
}

// disconnect marks the protocol disconnected and tears down the transport.
func (p *protocol) disconnect() {
	p.mu.Lock()
	p.connected = false
	p.mu.Unlock()
	_ = p.transport.Disconnect()
}

// dispatch sends a request and returns the raw result. If the server reports the
// session as expired it reconnects once and retries.
func (p *protocol) dispatch(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	needConnect := !p.connected && !p.connecting
	p.mu.Unlock()
	if needConnect {
		if err := p.connect(ctx); err != nil {
			return nil, err
		}
	}

	result, err := p.attempt(ctx, method, params)
	if errors.Is(err, errSessionExpired) {
		if err := p.connect(ctx); err != nil {
			return nil, err
		}
		return p.attempt(ctx, method, params)
	}
	return result, err
}

// attempt performs a single request/response exchange, answering any
// server-initiated requests interleaved before the matching response.
func (p *protocol) attempt(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	id := jsonrpc.IntID(p.nextID)
	p.nextID++
	p.mu.Unlock()

	req := jsonrpc.Request{JSONRPC: jsonrpc.Version, ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, wrapError(err, "unable to encode request params")
		}
		req.Params = raw
	}
	frame, err := json.Marshal(&req)
	if err != nil {
		return nil, wrapError(err, "unable to encode request")
	}

	if err := p.send(ctx, string(frame)); err != nil {
		return nil, err
	}

	for {
		raw, err := p.receive(ctx)
		if err != nil {
			return nil, err
		}

		if served, err := p.serveServerFrame(ctx, []byte(raw)); err != nil {
			return nil, err
		} else if served {
			continue
		}

		resp, perr := jsonrpc.ParseResponse([]byte(raw))
		if perr != nil {
			p.disconnect()
			return nil, newError("invalid JSON-RPC response from server: " + perr.Message)
		}
		if !bytes.Equal(resp.ID.Raw(), id.Raw()) {
			// A response for a different id; keep reading for ours.
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// serveServerFrame handles a frame initiated by the server. It answers ping
// requests, declines other requests with method-not-found, ignores
// notifications, and reports (false) for anything that is a client-bound
// response. The returned bool indicates the frame was consumed.
func (p *protocol) serveServerFrame(ctx context.Context, raw []byte) (bool, error) {
	var probe struct {
		Method *string         `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Method == nil {
		return false, nil
	}
	// A notification (no id) is simply ignored.
	if len(bytes.TrimSpace(probe.ID)) == 0 || bytes.Equal(bytes.TrimSpace(probe.ID), []byte("null")) {
		return true, nil
	}

	var id jsonrpc.ID
	_ = id.UnmarshalJSON(probe.ID)

	var resp *jsonrpc.Response
	if *probe.Method == "ping" {
		resp, _ = jsonrpc.NewResult(id, map[string]any{})
	} else {
		resp = jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeMethodNotFound,
			"method ["+*probe.Method+"] is not supported by this client")
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return true, wrapError(err, "unable to encode response to server request")
	}
	if err := p.send(ctx, string(out)); err != nil {
		return true, err
	}
	return true, nil
}

// notify sends a parameterless notification.
func (p *protocol) notify(ctx context.Context, method string) error {
	n, err := jsonrpc.NewNotification(method, nil)
	if err != nil {
		return wrapError(err, "unable to encode notification")
	}
	out, err := json.Marshal(n)
	if err != nil {
		return wrapError(err, "unable to encode notification")
	}
	return p.send(ctx, string(out))
}

// send transmits a frame, disconnecting on transport failure (including a
// session-expiry signal, so dispatch's retry performs a fresh handshake).
func (p *protocol) send(ctx context.Context, frame string) error {
	if err := p.transport.Send(ctx, frame); err != nil {
		p.disconnect()
		return err
	}
	return nil
}

// receive reads the next frame, disconnecting on transport failure.
func (p *protocol) receive(ctx context.Context) (string, error) {
	raw, err := p.transport.Receive(ctx)
	if err != nil {
		p.disconnect()
		return "", err
	}
	return raw, nil
}
