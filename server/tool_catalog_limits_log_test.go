package server_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity/contract"

	"github.com/velocitykode/velocity-mcp/server"
)

// warningLogger records the warnings the server logs, message and fields.
type warningLogger struct {
	mu       sync.Mutex
	warnings []string
}

func (l *warningLogger) Debug(string, ...any) {}
func (l *warningLogger) Info(string, ...any)  {}
func (l *warningLogger) Error(string, ...any) {}
func (l *warningLogger) Fatal(string, ...any) {}
func (l *warningLogger) With(kvs ...any) contract.Logger {
	return contract.BindFields(l, kvs...)
}
func (l *warningLogger) Warn(msg string, kvs ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, msg+" "+strings.TrimSpace(fmt.Sprintln(kvs...)))
}

func (l *warningLogger) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.warnings)
}

// WithToolCatalogLimits holds both limits between a floor and a ceiling and
// has no error to return. A limit that was moved is reported once through the
// server's logger, naming what was asked for and what applies; a limit taken
// as given is not reported at all.
func TestToolCatalogLimitsReportAnAdjustment(t *testing.T) {
	const adjusted = "mcp: tool catalog limits adjusted to their bounds "
	logger := &warningLogger{}
	withLogger := func() server.Option { return server.WithLogger(logger) }
	limits := server.WithToolCatalogLimits

	cases := []struct {
		name string
		opts func() []server.Option
		// want is the one warning expected; empty means none.
		want string
	}{
		{name: "limits within the bounds", opts: func() []server.Option { return []server.Option{withLogger(), limits(50, 1<<20)} }},
		{name: "limits on the bounds", opts: func() []server.Option {
			return []server.Option{withLogger(), limits(100, 4<<20), limits(1, 512)}
		}},
		{name: "no limits configured", opts: func() []server.Option { return []server.Option{withLogger()} }},
		{name: "calls above the ceiling", opts: func() []server.Option { return []server.Option{withLogger(), limits(500, 1<<20)} },
			want: adjusted + "requested_max_tool_calls 500 max_tool_calls 100 requested_max_output_bytes 1048576 max_output_bytes 1048576"},
		{name: "bytes above the ceiling", opts: func() []server.Option { return []server.Option{withLogger(), limits(50, 64<<20)} },
			want: adjusted + "requested_max_tool_calls 50 max_tool_calls 50 requested_max_output_bytes 67108864 max_output_bytes 4194304"},
		{name: "both above the ceilings, reported together", opts: func() []server.Option { return []server.Option{withLogger(), limits(101, 4<<20+1)} },
			want: adjusted + "requested_max_tool_calls 101 max_tool_calls 100 requested_max_output_bytes 4194305 max_output_bytes 4194304"},
		{name: "both below the floors", opts: func() []server.Option { return []server.Option{withLogger(), limits(0, -1)} },
			want: adjusted + "requested_max_tool_calls 0 max_tool_calls 1 requested_max_output_bytes -1 max_output_bytes 512"},
		{name: "the logger set after the limits", opts: func() []server.Option { return []server.Option{limits(500, 1<<20), withLogger()} },
			want: adjusted + "requested_max_tool_calls 500 max_tool_calls 100 requested_max_output_bytes 1048576 max_output_bytes 1048576"},
		{name: "alongside a catalog", opts: func() []server.Option {
			return []server.Option{withLogger(), server.WithToolCatalog(server.NewTool("echo", "Echoes")), limits(500, 1<<20)}
		}, want: adjusted + "requested_max_tool_calls 500 max_tool_calls 100 requested_max_output_bytes 1048576 max_output_bytes 1048576"},
		{name: "only the limits that stand are reported", opts: func() []server.Option {
			return []server.Option{withLogger(), limits(500, 1<<20), limits(900, 1<<20)}
		}, want: adjusted + "requested_max_tool_calls 900 max_tool_calls 100 requested_max_output_bytes 1048576 max_output_bytes 1048576"},
		{name: "adjusted limits replaced by ones within the bounds", opts: func() []server.Option {
			return []server.Option{withLogger(), limits(500, 64<<20), limits(50, 1<<20)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger.mu.Lock()
			logger.warnings = nil
			logger.mu.Unlock()

			server.New("demo", "1.0.0", tc.opts()...)

			got := logger.recorded()
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("warnings = %q, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("warnings = %q, want exactly %q", got, tc.want)
			}
		})
	}

	// Without a logger the adjustment has nowhere to go, and must not be a
	// reason to fail.
	server.New("demo", "1.0.0", limits(500, 64<<20))
}
