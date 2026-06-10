package mcptest_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/mcptest"
	_ "github.com/velocitykode/velocity-mcp/server/methods"
)

// fakeTB is a minimal testing.TB stub that records Fatalf as a failure (via a
// runtime.Goexit, mirroring testing.T.FailNow) so the negative-path assertions
// can be exercised without aborting the real test. It implements only the
// surface mcptest uses: Helper, Fatalf, Cleanup.
type fakeTB struct {
	testing.TB
	mu       sync.Mutex
	failed   bool
	messages []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Cleanup(func()) {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.mu.Lock()
	f.failed = true
	f.messages = append(f.messages, sprintf(format, args...))
	f.mu.Unlock()
	// Mirror testing.T.FailNow: abandon the current goroutine so no code after
	// the failing assertion runs. The driver goroutine (run below) recovers via
	// Goexit's deferred-only semantics.
	runtimeGoexit()
}

func (f *fakeTB) didFail() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

func (f *fakeTB) lastMessage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return ""
	}
	return f.messages[len(f.messages)-1]
}

// runAssertion runs fn on a dedicated goroutine so a Fatalf-driven Goexit
// terminates only that goroutine, then reports whether fn triggered a failure on
// tb. It mirrors how testing isolates a FailNow.
func runAssertion(tb *fakeTB, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
	return tb.didFail()
}

func TestAssertions_NegativePaths(t *testing.T) {
	tests := []struct {
		name    string
		assert  func(ts *mcptest.Server)
		wantSub string
	}{
		{
			name: "AssertOk on tool error",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("boom", nil).AssertOk()
			},
			wantSub: "expected no errors",
		},
		{
			name: "AssertError when ok",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertError()
			},
			wantSub: "expected an error",
		},
		{
			name: "AssertError message mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("boom", nil).AssertError("not-the-message")
			},
			wantSub: "expected error containing",
		},
		{
			name: "AssertErrorCode no error",
			assert: func(ts *mcptest.Server) {
				ts.Ping().AssertErrorCode(jsonrpc.CodeInvalidParams)
			},
			wantSub: "expected a protocol error",
		},
		{
			name: "AssertErrorCode wrong code",
			assert: func(ts *mcptest.Server) {
				ts.Send("missing/method", nil).AssertErrorCode(jsonrpc.CodeInvalidParams)
			},
			wantSub: "expected error code",
		},
		{
			name: "AssertText missing",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertText("999")
			},
			wantSub: "expected to see",
		},
		{
			name: "AssertDontSeeText present",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("add", map[string]any{"a": 2, "b": 3}).AssertDontSeeText("5")
			},
			wantSub: "did not expect to see",
		},
		{
			name: "AssertHasResponse none",
			assert: func(ts *mcptest.Server) {
				ts.Notify("notifications/initialized", nil).AssertHasResponse()
			},
			wantSub: "expected a response",
		},
		{
			name: "AssertNoResponse has one",
			assert: func(ts *mcptest.Server) {
				ts.Ping().AssertNoResponse()
			},
			wantSub: "expected no response",
		},
		{
			name: "AssertResult missing key",
			assert: func(ts *mcptest.Server) {
				ts.Ping().AssertResult("nope", 1)
			},
			wantSub: "missing key",
		},
		{
			name: "AssertResult wrong value",
			assert: func(ts *mcptest.Server) {
				ts.Initialize().AssertResult("protocolVersion", "1999-01-01")
			},
			wantSub: "want",
		},
		{
			name: "AssertResult no result object",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("does-not-exist", nil).AssertResult("x", 1)
			},
			wantSub: "no result",
		},
		{
			name: "AssertProtocolVersion mismatch",
			assert: func(ts *mcptest.Server) {
				ts.Initialize().AssertProtocolVersion("1999-01-01")
			},
			wantSub: "want",
		},
		{
			name: "AssertServerName mismatch",
			assert: func(ts *mcptest.Server) {
				ts.Initialize().AssertServerName("wrong")
			},
			wantSub: "serverInfo.name",
		},
		{
			name: "AssertToolListed missing",
			assert: func(ts *mcptest.Server) {
				ts.ListTools().AssertToolListed("ghost")
			},
			wantSub: "was not listed",
		},
		{
			name: "AssertToolNotListed present",
			assert: func(ts *mcptest.Server) {
				ts.ListTools().AssertToolNotListed("add")
			},
			wantSub: "should not have been",
		},
		{
			name: "AssertToolCount mismatch",
			assert: func(ts *mcptest.Server) {
				ts.ListTools().AssertToolCount(99)
			},
			wantSub: "want 99",
		},
		{
			name: "AssertResourceListed missing",
			assert: func(ts *mcptest.Server) {
				ts.ListResources().AssertResourceListed("ghost")
			},
			wantSub: "was not listed",
		},
		{
			name: "AssertPromptListed missing",
			assert: func(ts *mcptest.Server) {
				ts.ListPrompts().AssertPromptListed("ghost")
			},
			wantSub: "was not listed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := &fakeTB{}
			ts := mcptest.NewServer(tb, demoServer())
			// Drive the (possibly stateful) initialize first where needed; most
			// assertions stand alone, so just run the assertion.
			failed := runAssertion(tb, func() { tt.assert(ts) })
			if !failed {
				t.Fatalf("expected assertion %q to fail", tt.name)
			}
			if msg := tb.lastMessage(); !strings.Contains(msg, tt.wantSub) {
				t.Fatalf("failure message %q does not contain %q", msg, tt.wantSub)
			}
		})
	}
}

func TestAssertions_PositiveChaining(t *testing.T) {
	// A single chain exercising the happy path of many assertions at once.
	ts := mcptest.NewServer(t, demoServer())
	ts.Initialize().
		AssertOk().
		AssertHasResponse().
		AssertProtocolVersion("2025-06-18").
		AssertServerName("demo").
		AssertResult("instructions", "demo instructions")

	ts.CallTool("add", map[string]any{"a": 10, "b": 5}).
		AssertOk().
		AssertText("15").
		AssertDontSeeText("999")

	ts.ListTools().
		AssertOk().
		AssertToolListed("add").
		AssertToolNotListed("ghost").
		AssertToolCount(2)
}
