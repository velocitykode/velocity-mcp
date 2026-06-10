package transport

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

func TestFake_Run_NoOp(t *testing.T) {
	f := NewFake(nil)
	if err := f.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var nilCtx context.Context // nil-valued context, exercising the tolerance branch
	if err := f.Run(nilCtx); err != nil {
		t.Fatalf("Run(nil): %v", err)
	}
}

func TestFake_Run_CancelledContext(t *testing.T) {
	f := NewFake(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Run(ctx); err == nil {
		t.Fatal("Run should surface a cancelled context error")
	}
}

func TestFake_Send_Records(t *testing.T) {
	f := NewFake(nil)
	if err := f.Send(context.Background(), []byte(`{"a":1}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := f.Send(context.Background(), []byte(`{"b":2}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := f.Sent()
	if len(sent) != 2 {
		t.Fatalf("want 2 sent, got %d", len(sent))
	}
	if !bytes.Equal(sent[0], []byte(`{"a":1}`)) || !bytes.Equal(sent[1], []byte(`{"b":2}`)) {
		t.Fatalf("recorded frames wrong: %q", sent)
	}
	if !bytes.Equal(f.LastSent(), []byte(`{"b":2}`)) {
		t.Fatalf("LastSent wrong: %q", f.LastSent())
	}
}

func TestFake_Send_CopiesInput(t *testing.T) {
	f := NewFake(nil)
	buf := []byte(`{"a":1}`)
	if err := f.Send(context.Background(), buf); err != nil {
		t.Fatalf("Send: %v", err)
	}
	buf[2] = 'X' // mutate caller's buffer after Send
	if !bytes.Equal(f.Sent()[0], []byte(`{"a":1}`)) {
		t.Fatalf("Send did not copy input: %q", f.Sent()[0])
	}
}

func TestFake_LastSent_Empty(t *testing.T) {
	if got := NewFake(nil).LastSent(); got != nil {
		t.Fatalf("LastSent on empty fake should be nil, got %q", got)
	}
}

func TestFake_Inject_NoServer(t *testing.T) {
	f := NewFake(nil)
	reply, err := f.Inject(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if reply != nil {
		t.Fatalf("nil-server Inject should return nil reply, got %q", reply)
	}
	// The inbound frame is still recorded.
	if len(f.Received()) != 1 {
		t.Fatalf("want 1 received, got %d", len(f.Received()))
	}
	if len(f.Sent()) != 0 {
		t.Fatalf("nil-server Inject should send nothing, got %d", len(f.Sent()))
	}
}

func TestFake_Inject_DrivesServer(t *testing.T) {
	srv := newTestServer(t)
	f := NewFake(srv)

	// initialize assigns a session id.
	reply, err := f.Inject(context.Background(), initializeRequest(1))
	if err != nil {
		t.Fatalf("Inject initialize: %v", err)
	}
	resp := decodeResponse(t, reply)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	if f.SessionID() != "fixed-session" {
		t.Fatalf("session id not retained: %q", f.SessionID())
	}

	// tools/call should carry the retained session id and return the sum.
	reply, err = f.Inject(context.Background(), callToolRequest(2, 4, 5))
	if err != nil {
		t.Fatalf("Inject call: %v", err)
	}
	resp = decodeResponse(t, reply)
	if resp.Error != nil {
		t.Fatalf("tools/call errored: %+v", resp.Error)
	}
	if !bytes.Contains(resp.Result, []byte("9")) {
		t.Fatalf("tools/call result missing sum: %s", resp.Result)
	}

	// Both replies were recorded as sent frames.
	if len(f.Sent()) != 2 {
		t.Fatalf("want 2 sent frames, got %d", len(f.Sent()))
	}
	if len(f.Received()) != 2 {
		t.Fatalf("want 2 received frames, got %d", len(f.Received()))
	}
}

func TestFake_Inject_Notification_NoReply(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{HasResponse: false}
	}}
	f := NewFake(stub)
	reply, err := f.Inject(context.Background(), []byte(`{"jsonrpc":"2.0","method":"x"}`))
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if reply != nil {
		t.Fatalf("notification should yield nil reply, got %q", reply)
	}
	if len(f.Sent()) != 0 {
		t.Fatalf("notification should not record a sent frame")
	}
}

func TestFake_Inject_NilContext(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		if ctx == nil {
			t.Error("Inject must not pass a nil context to Handle")
		}
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true}
	}}
	f := NewFake(stub)
	var nilCtx context.Context // nil-valued context, exercising the tolerance branch
	if _, err := f.Inject(nilCtx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("Inject(nil ctx): %v", err)
	}
}

func TestFake_Inject_MarshalErrorSurfaces(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		// A response whose error data cannot be JSON marshalled.
		resp := jsonrpc.NewErrorResponse(jsonrpc.IntID(1), jsonrpc.NewError(jsonrpc.CodeInternalError, "x").WithData(func() {}))
		return server.HandleResult{Response: resp, HasResponse: true}
	}}
	f := NewFake(stub)
	reply, err := f.Inject(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil {
		t.Fatal("Inject should surface an encode error")
	}
	if reply != nil {
		t.Fatalf("failed Inject should return nil reply, got %q", reply)
	}
	if len(f.Sent()) != 0 {
		t.Fatalf("failed encode should record no sent frame, got %d", len(f.Sent()))
	}
}

func TestFake_Reset(t *testing.T) {
	srv := newTestServer(t)
	f := NewFake(srv)
	if _, err := f.Inject(context.Background(), initializeRequest(1)); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	f.Reset()
	if len(f.Sent()) != 0 || len(f.Received()) != 0 || f.SessionID() != "" {
		t.Fatalf("Reset did not clear state: sent=%d received=%d session=%q", len(f.Sent()), len(f.Received()), f.SessionID())
	}
}

func TestFake_Accessors_ReturnCopies(t *testing.T) {
	f := NewFake(nil)
	_ = f.Send(context.Background(), []byte(`{"a":1}`))
	_, _ = f.Inject(context.Background(), []byte(`{"r":1}`))

	sent := f.Sent()
	sent[0][0] = 'Z'
	if f.Sent()[0][0] == 'Z' {
		t.Fatal("Sent() returned an aliased slice")
	}
	recv := f.Received()
	recv[0][0] = 'Z'
	if f.Received()[0][0] == 'Z' {
		t.Fatal("Received() returned an aliased slice")
	}
}

func TestFake_EmptyAccessors(t *testing.T) {
	f := NewFake(nil)
	if f.Sent() != nil {
		t.Fatal("Sent() on empty fake should be nil")
	}
	if f.Received() != nil {
		t.Fatal("Received() on empty fake should be nil")
	}
}

func TestFake_Concurrent(t *testing.T) {
	stub := &stubServer{fn: func(ctx context.Context, raw []byte, sessionID string) server.HandleResult {
		return server.HandleResult{Response: mustResult(t, jsonrpc.IntID(1), nil), HasResponse: true}
	}}
	f := NewFake(stub)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Send(context.Background(), []byte(`{"s":1}`))
			_, _ = f.Inject(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			_ = f.Sent()
			_ = f.Received()
			_ = f.SessionID()
		}()
	}
	wg.Wait()
	// 50 Send + 50 Inject replies = 100 sent frames.
	if got := len(f.Sent()); got != 100 {
		t.Fatalf("want 100 sent frames, got %d", got)
	}
}
