package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

func TestRequestMerge(t *testing.T) {
	req := NewRequest(map[string]any{"a": 1})
	req.merge(map[string]any{"b": 2})
	if req.Get("b") != 2 {
		t.Fatalf("merge failed: %v", req.All())
	}
	// nil merge is a no-op.
	req.merge(nil)
	if len(req.All()) != 2 {
		t.Fatalf("nil merge changed args: %v", req.All())
	}

	// merge initializes a nil args map.
	r2 := &Request{}
	r2.merge(map[string]any{"x": 9})
	if r2.Get("x") != 9 {
		t.Fatal("merge should initialize nil args")
	}
}

// captureLogger records error lines for assertions.
type captureLogger struct{ errors int }

func (l *captureLogger) Debug(string, ...any) {}
func (l *captureLogger) Info(string, ...any)  {}
func (l *captureLogger) Warn(string, ...any)  {}
func (l *captureLogger) Error(string, ...any) { l.errors++ }
func (l *captureLogger) Fatal(string, ...any) {}

func TestWithLoggerAndDispatchError(t *testing.T) {
	lg := &captureLogger{}
	s := New("demo", "1.0.0", WithLogger(lg))
	s.SetEventDispatcher(func(context.Context, any) error {
		return context.Canceled
	})
	s.dispatch(context.Background(), "x")
	if lg.errors != 1 {
		t.Fatalf("expected 1 logged error, got %d", lg.errors)
	}
}

func TestInitializeMethodFallback(t *testing.T) {
	// Directly exercise the built-in fallback handler (the methods package
	// override is installed binary-wide, so call the fallback type directly).
	c := New("demo", "1.0.0").createContext(context.Background(), "")
	rawParams, _ := json.Marshal(map[string]any{"protocolVersion": "2025-06-18"})
	req := &jsonrpc.Request{
		JSONRPC: jsonrpc.Version,
		ID:      jsonrpc.IntID(1),
		Method:  "initialize",
		Params:  rawParams,
	}
	resp, err := initializeMethod{}.Handle(c, req)
	if err != nil || resp == nil || resp.Error != nil {
		t.Fatalf("fallback initialize failed: resp=%+v err=%v", resp, err)
	}
}

func TestToFloatAllKinds(t *testing.T) {
	cases := []any{int8(1), int16(2), int32(3), uint8(4), uint16(5), uint32(6), uint64(7)}
	for _, c := range cases {
		if _, ok := toFloat(c); !ok {
			t.Fatalf("toFloat(%T) should be ok", c)
		}
	}
}
