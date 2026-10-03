package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file covers what the stdio transport keeps of a subprocess's output. A
// server writes to standard error for as long as it runs, which the MCP
// specification (basic/transports/stdio) makes the place for its logging, and
// to standard output whatever it likes: neither may grow the client without
// bound.

// stderrCeiling and frameCeiling are the bounds the transport is held to here:
// the tail of standard error it keeps, and the largest frame it reads.
const (
	stderrCeiling = 64 << 10
	frameCeiling  = 32 << 20
)

// TestStdioKeepsABoundedTailOfStandardError asserts a server that logs a great
// deal costs the client a bounded amount of memory, and that what is kept is
// the end of the log: the last thing a server said is what explains why it
// ended.
func TestStdioKeepsABoundedTailOfStandardError(t *testing.T) {
	const logged = 4 << 20
	script := `head -c ` + strconv.Itoa(logged) + ` /dev/zero | tr '\0' 'x' >&2; echo 'fatal: out of patience' >&2; ` +
		`IFS= read -r line; exit 3`
	tr := NewStdioTransport("/bin/sh", "-c", script)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	held := func() string {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.stderr.String()
	}
	if !pollFor(10*time.Second, func() bool { return strings.HasSuffix(held(), "fatal: out of patience\n") }) {
		t.Fatalf("the end of the log never arrived; %d bytes held", len(held()))
	}
	if got := len(held()); got > stderrCeiling {
		t.Fatalf("the client holds %d bytes of standard error, want at most %d", got, stderrCeiling)
	}

	// The subprocess ends on the frame it is sent, and the report of that
	// carries what it said last, within the same bound.
	if err := tr.Send(context.Background(), `{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err := tr.Receive(context.Background())
	var transportErr *TransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("error = %v (%T), want a transport failure", err, err)
	}
	if !strings.HasSuffix(err.Error(), "fatal: out of patience") {
		t.Fatalf("the report does not end with what the server said last: %.120q", err.Error())
	}
	if got := len(err.Error()); got > stderrCeiling+512 {
		t.Fatalf("the report is %d bytes long, want it bounded by the %d kept", got, stderrCeiling)
	}
}

// TestStdioRefusesAFrameItCannotBound asserts a line that never ends is not
// read for ever: past the limit the reading stops, the failure is the channel's,
// and the subprocess is given up.
func TestStdioRefusesAFrameItCannotBound(t *testing.T) {
	oversized := strconv.Itoa(frameCeiling + 1<<20)
	tests := []struct {
		name   string
		script string
		// wantFrame is the frame delivered before the failure, if any.
		wantFrame string
		wantErr   string
	}{
		{
			name:    "a line past the limit ends the reading",
			script:  `head -c ` + oversized + ` /dev/zero | tr '\0' 'x'; echo; exec cat > /dev/null`,
			wantErr: "sent a frame this client does not read to its end",
		},
		{
			name:      "a frame within the limit is delivered whole",
			script:    `head -c 60000 /dev/zero | tr '\0' 'x'; echo; exec cat > /dev/null`,
			wantFrame: strings.Repeat("x", 60000),
		},
		{
			name:      "the frames before an oversized one are still delivered",
			script:    `echo first; head -c ` + oversized + ` /dev/zero | tr '\0' 'x'; echo; exec cat > /dev/null`,
			wantFrame: "first",
			wantErr:   "sent a frame this client does not read to its end",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewStdioTransport("/bin/sh", "-c", tt.script)
			tr.SetTimeout(20 * time.Second)
			if err := tr.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer tr.Disconnect()

			if tt.wantFrame != "" {
				frame, err := tr.Receive(context.Background())
				if err != nil {
					t.Fatalf("receive: %v", err)
				}
				if frame != tt.wantFrame {
					t.Fatalf("frame of %d bytes, want the %d sent", len(frame), len(tt.wantFrame))
				}
			}
			if tt.wantErr == "" {
				return
			}
			frame, err := tr.Receive(context.Background())
			if err == nil {
				t.Fatalf("a frame of %d bytes was delivered past a limit of %d", len(frame), frameCeiling)
			}
			var transportErr *TransportError
			if !errors.As(err, &transportErr) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v (%T), want a transport failure saying %q", err, err, tt.wantErr)
			}
			assertStdioTornDown(t, tr)
		})
	}
}

// TestStdioHoldsABoundedAmountOfUnreadOutput asserts a server that writes while
// nobody receives cannot make the client hold more than the frame bound of
// what it wrote: once that much is read and not yet received, the reader takes
// no more, and the server blocks on its pipe as it would with no reader at all.
// The server here records how many frames it managed to write; each is a
// quarter of the bound, so the count says how much the client took.
func TestStdioHoldsABoundedAmountOfUnreadOutput(t *testing.T) {
	const frame = frameCeiling / 4
	written := filepath.Join(t.TempDir(), "written")
	script := `blob=$(head -c ` + strconv.Itoa(frame-1) + ` /dev/zero | tr '\0' 'x'); i=0; ` +
		`while [ $i -lt 24 ]; do printf '%s\n' "$blob"; i=$((i+1)); echo $i > '` + written + `'; done; exit 0`
	tr := NewStdioTransport("/bin/sh", "-c", script)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Disconnect()

	count := func() int {
		raw, err := os.ReadFile(written)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return n
	}
	// The server writes until it blocks; the count is read once it has stood
	// still for a while.
	last, stillSince := -1, time.Now()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if n := count(); n != last {
			last, stillSince = n, time.Now()
		} else if time.Since(stillSince) > time.Second {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("the server wrote %d frames of %d bytes before it blocked", last, frame)
	// The bound in the buffer, one frame in the reader's hands, and one the
	// pipe may hold part of.
	if last > 6 {
		t.Fatalf("the server wrote %d frames nobody received, want at most 6: the client holds about %d MiB of them",
			last, last*frame>>20)
	}
}
