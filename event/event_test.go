package event

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// namer mirrors velocity's events.Event interface (Name() string) so we can
// assert, without importing the events package, that every event type here is
// dispatchable through Velocity's event system.
type namer interface {
	Name() string
}

func TestEvent_Name(t *testing.T) {
	tests := []struct {
		name  string
		event namer
		want  string
	}{
		{"session initialized", SessionInitialized{}, "mcp.session.initialized"},
		{"tool called", ToolCalled{}, "mcp.tool.called"},
		{"tool failed", ToolFailed{}, "mcp.tool.failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.Name(); got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEvent_NameConstants(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"NameSessionInitialized", NameSessionInitialized, "mcp.session.initialized"},
		{"NameToolCalled", NameToolCalled, "mcp.tool.called"},
		{"NameToolFailed", NameToolFailed, "mcp.tool.failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestEvent_NamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range []string{NameSessionInitialized, NameToolCalled, NameToolFailed} {
		if seen[n] {
			t.Errorf("duplicate event name %q", n)
		}
		seen[n] = true
	}
}

func TestSessionInitialized_ClientAccessors(t *testing.T) {
	tests := []struct {
		name                             string
		event                            SessionInitialized
		wantName, wantTitle, wantVersion string
	}{
		{
			name: "full client info",
			event: SessionInitialized{
				ClientInfo: &ClientInfo{Name: "demo", Title: "Demo Client", Version: "1.2.3"},
			},
			wantName:    "demo",
			wantTitle:   "Demo Client",
			wantVersion: "1.2.3",
		},
		{
			name:        "nil client info",
			event:       SessionInitialized{ClientInfo: nil},
			wantName:    "",
			wantTitle:   "",
			wantVersion: "",
		},
		{
			name:        "partial client info",
			event:       SessionInitialized{ClientInfo: &ClientInfo{Name: "only-name"}},
			wantName:    "only-name",
			wantTitle:   "",
			wantVersion: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.ClientName(); got != tt.wantName {
				t.Errorf("ClientName() = %q, want %q", got, tt.wantName)
			}
			if got := tt.event.ClientTitle(); got != tt.wantTitle {
				t.Errorf("ClientTitle() = %q, want %q", got, tt.wantTitle)
			}
			if got := tt.event.ClientVersion(); got != tt.wantVersion {
				t.Errorf("ClientVersion() = %q, want %q", got, tt.wantVersion)
			}
		})
	}
}

func TestSessionInitialized_Fields(t *testing.T) {
	caps := map[string]any{"tools": map[string]any{"listChanged": true}}
	e := SessionInitialized{
		SessionID:          "sess-1",
		ClientInfo:         &ClientInfo{Name: "demo"},
		ProtocolVersion:    "2025-06-18",
		ClientCapabilities: caps,
	}
	if e.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", e.SessionID, "sess-1")
	}
	if e.ProtocolVersion != "2025-06-18" {
		t.Errorf("ProtocolVersion = %q, want %q", e.ProtocolVersion, "2025-06-18")
	}
	if e.ClientCapabilities["tools"] == nil {
		t.Errorf("ClientCapabilities lost the tools entry: %#v", e.ClientCapabilities)
	}
}

func TestToolCalled_Fields(t *testing.T) {
	args := map[string]any{"a": 1, "b": 2}
	e := ToolCalled{
		SessionID: "sess-2",
		Tool:      "add",
		Arguments: args,
		IsError:   true,
		Duration:  150 * time.Millisecond,
	}
	if e.SessionID != "sess-2" {
		t.Errorf("SessionID = %q, want %q", e.SessionID, "sess-2")
	}
	if e.Tool != "add" {
		t.Errorf("Tool = %q, want %q", e.Tool, "add")
	}
	if !e.IsError {
		t.Error("IsError = false, want true")
	}
	if e.Duration != 150*time.Millisecond {
		t.Errorf("Duration = %v, want %v", e.Duration, 150*time.Millisecond)
	}
	if len(e.Arguments) != 2 {
		t.Errorf("Arguments len = %d, want 2", len(e.Arguments))
	}
}

func TestToolFailed_Fields(t *testing.T) {
	sentinel := errors.New("boom")
	e := ToolFailed{
		SessionID: "sess-3",
		Tool:      "divide",
		Arguments: map[string]any{"x": 1, "y": 0},
		Err:       sentinel,
		Duration:  2 * time.Second,
	}
	if e.SessionID != "sess-3" {
		t.Errorf("SessionID = %q, want %q", e.SessionID, "sess-3")
	}
	if e.Tool != "divide" {
		t.Errorf("Tool = %q, want %q", e.Tool, "divide")
	}
	if !errors.Is(e.Err, sentinel) {
		t.Errorf("Err = %v, want %v", e.Err, sentinel)
	}
	if e.Duration != 2*time.Second {
		t.Errorf("Duration = %v, want %v", e.Duration, 2*time.Second)
	}
}

func TestToolFailed_NilErr(t *testing.T) {
	e := ToolFailed{Tool: "noop"}
	if e.Err != nil {
		t.Errorf("Err = %v, want nil", e.Err)
	}
}

// TestEvent_Concurrent verifies the event values are safe to read from multiple
// goroutines. They are plain immutable data once constructed, so concurrent
// reads of their fields and Name()/accessor methods must not race.
func TestEvent_Concurrent(t *testing.T) {
	si := SessionInitialized{
		SessionID:          "sess",
		ClientInfo:         &ClientInfo{Name: "demo", Title: "Demo", Version: "1.0.0"},
		ProtocolVersion:    "2025-06-18",
		ClientCapabilities: map[string]any{"tools": true},
	}
	tc := ToolCalled{SessionID: "sess", Tool: "add", IsError: false, Duration: time.Second}
	tf := ToolFailed{SessionID: "sess", Tool: "add", Err: errors.New("x"), Duration: time.Second}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = si.Name()
			_ = si.ClientName()
			_ = si.ClientTitle()
			_ = si.ClientVersion()
			_ = tc.Name()
			_ = tf.Name()
		}()
	}
	wg.Wait()
}
