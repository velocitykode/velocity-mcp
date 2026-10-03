package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file covers how the stdio transport stops the subprocess it started.
// The MCP specification (basic/lifecycle#shutdown) has the client close the
// input stream of the server, wait for it to exit, ask it to terminate when it
// does not within a reasonable time, and kill it only when it still does not.

// pollFor waits for a condition, polling, and reports whether it came true
// within the ceiling.
func pollFor(ceiling time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(ceiling)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

// processGone reports whether the process recorded in a pid file has ended.
func processGone(t *testing.T, pidFile string) bool {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid file holds %q", raw)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return process.Signal(syscall.Signal(0)) != nil
}

// TestStdioDisconnectStopsTheServerInStages asserts each stage of the shutdown
// and the bound on the whole of it: a server is given the time to exit by
// itself, one that does not is asked before it is killed, and no server and no
// descendant of one holds the disconnect past its bound.
func TestStdioDisconnectStopsTheServerInStages(t *testing.T) {
	tests := []struct {
		name string
		// script is run by /bin/sh; %[1]s is the marker file and %[2]s the pid
		// file. It records its pid before anything else.
		script string
		// ceiling is how long the disconnect may take.
		ceiling time.Duration
		// wantMarker is what the server must have written by the time the
		// disconnect returns, when it is given the chance to.
		wantMarker string
	}{
		{
			name: "a server that needs a moment to finish is given it",
			script: `echo $$ > %[2]q; while IFS= read -r line; do :; done; ` +
				`sleep 0.3; echo flushed > %[1]q`,
			ceiling:    1500 * time.Millisecond,
			wantMarker: "flushed",
		},
		{
			name: "a descendant holding the pipes does not hold the disconnect",
			script: `echo $$ > %[2]q; sleep 20 & while IFS= read -r line; do :; done; ` +
				`echo flushed > %[1]q`,
			ceiling:    4 * time.Second,
			wantMarker: "flushed",
		},
		{
			name: "a server that ignores the end of its input is asked to terminate",
			script: `echo $$ > %[2]q; trap 'echo terminated > %[1]q; exit 0' TERM; ` +
				`while :; do sleep 0.1; done`,
			ceiling:    5 * time.Second,
			wantMarker: "terminated",
		},
		{
			name:    "a server that ignores being asked is killed",
			script:  `echo $$ > %[2]q; trap '' TERM; while :; do sleep 0.1; done`,
			ceiling: 8 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			marker, pidFile := filepath.Join(dir, "marker"), filepath.Join(dir, "pid")
			script := strings.NewReplacer("%[1]q", "'"+marker+"'", "%[2]q", "'"+pidFile+"'").Replace(tt.script)

			tr := NewStdioTransport("/bin/sh", "-c", script)
			if err := tr.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			// The server is up once it has recorded its pid, so the disconnect
			// meets a running server rather than a shell still starting.
			if !pollFor(10*time.Second, func() bool {
				raw, err := os.ReadFile(pidFile)
				return err == nil && strings.TrimSpace(string(raw)) != ""
			}) {
				_ = tr.Disconnect()
				t.Fatal("the server never started")
			}

			returned := make(chan time.Duration, 1)
			start := time.Now()
			go func() {
				_ = tr.Disconnect()
				returned <- time.Since(start)
			}()
			select {
			case took := <-returned:
				t.Logf("disconnect took %v", took)
			case <-time.After(tt.ceiling):
				t.Fatalf("the disconnect had not returned after %v", tt.ceiling)
			}

			if !processGone(t, pidFile) {
				t.Fatal("the server outlived the disconnect")
			}
			if tt.wantMarker == "" {
				return
			}
			raw, err := os.ReadFile(marker)
			if err != nil || strings.TrimSpace(string(raw)) != tt.wantMarker {
				t.Fatalf("marker = %q (%v), want %q: the server was stopped before it could finish", raw, err, tt.wantMarker)
			}
		})
	}
}

// TestStdioReaderEndsWithTheSubprocess asserts the goroutine reading the
// subprocess's output does not outlive a disconnect, however many frames the
// server had written that nobody was going to read.
func TestStdioReaderEndsWithTheSubprocess(t *testing.T) {
	readers := func() int {
		var dump strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 2)
		return strings.Count(dump.String(), "(*StdioTransport).readLoop")
	}
	before := readers()

	// Far more frames than the transport buffers, then a server that exits when
	// its input ends.
	script := `i=0; while [ $i -lt 200 ]; do ` +
		`echo '{"jsonrpc":"2.0","method":"notifications/message","params":{}}'; i=$((i+1)); done; exec cat > /dev/null`
	for range 5 {
		tr := NewStdioTransport("/bin/sh", "-c", script)
		if err := tr.Connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		tr.mu.Lock()
		lines := tr.lines
		tr.mu.Unlock()
		// The reader is parked on a full buffer, which is where it used to stay.
		if !pollFor(10*time.Second, func() bool { return len(lines) == cap(lines) }) {
			_ = tr.Disconnect()
			t.Fatalf("the server wrote only %d frames", len(lines))
		}
		if err := tr.Disconnect(); err != nil {
			t.Fatalf("disconnect: %v", err)
		}
	}

	if !pollFor(5*time.Second, func() bool { return readers() <= before }) {
		t.Fatalf("%d reader goroutine(s) outlived their subprocess", readers()-before)
	}
}

// TestStdioDisconnectReleasesAReceiveInFlight asserts a wait for a frame ends
// when the transport is disconnected underneath it, and says why.
func TestStdioDisconnectReleasesAReceiveInFlight(t *testing.T) {
	tr := NewStdioTransport("cat")
	tr.SetTimeout(30 * time.Second)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	received := make(chan error, 1)
	go func() {
		_, err := tr.Receive(context.Background())
		received <- err
	}()
	// The disconnect has to meet the wait, not come before it.
	waiting := func() bool {
		var dump strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 2)
		return strings.Contains(dump.String(), "(*StdioTransport).Receive")
	}
	if !pollFor(5*time.Second, waiting) {
		t.Fatal("the receive never started waiting")
	}
	if err := tr.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	select {
	case err := <-received:
		if err == nil || err.Error() != "transport is not connected" {
			t.Fatalf("receive = %v, want it told the transport is not connected", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a receive in flight outlived the disconnect")
	}
}
