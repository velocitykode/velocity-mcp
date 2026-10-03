package client

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// shutdownGrace is how long each stage of stopping the subprocess is given. The
// MCP specification (basic/lifecycle#shutdown) has a client close the input
// stream of the server, wait for it to exit, ask it to terminate if it has not
// within a reasonable time, and kill it only if it still has not.
const shutdownGrace = 2 * time.Second

// pipeGrace is how long the output of a subprocess that has exited is still
// waited for. A descendant it left running holds the pipes open for as long as
// it lives, and a disconnect must not wait on a process it never started.
const pipeGrace = time.Second

// StdioTransport speaks newline-delimited JSON-RPC to a server subprocess over
// its stdin/stdout. A background reader drains stdout into a buffered channel so
// Receive can honour context cancellation and the configured timeout without
// blocking on the pipe.
type StdioTransport struct {
	command string
	args    []string

	mu      sync.Mutex
	timeout time.Duration
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *tailBuffer
	lines   chan string
	readErr chan error
	// unread counts what was read from the subprocess and not yet received,
	// and holds the reader once it reaches the frame bound.
	unread *backlog
	// done is closed when the subprocess is given up, which releases whoever
	// is waiting on its output: the reader, and a Receive in flight.
	done chan struct{}
}

// maxStderrBytes caps how much of the subprocess's standard error is kept. A
// server logs there by design, for as long as it runs, and the only reader is
// the report of a subprocess that ended early, which wants the last thing it
// said: the tail is kept and everything before it let go.
const maxStderrBytes = 64 << 10

// maxFrameBytes caps one frame read from the subprocess, and how much of what
// it wrote is held read and not yet received. It is the bound the HTTP
// transport already reads a response body under, so a server cannot make the
// client buffer more over one channel than over the other: what the client
// holds of a server writing while nobody receives is that much in the buffer
// and the one frame in the reader's hands.
const maxFrameBytes = 32 << 20

// backlog counts the bytes of the frames read from a subprocess and not yet
// received. The reader takes a frame only while what it holds is under the
// bound, and otherwise waits for a Receive to make room, so a server that
// writes while nobody receives blocks on its pipe as it would with no reader
// at all, rather than filling the client's memory.
type backlog struct {
	mu     sync.Mutex
	room   sync.Cond
	held   int
	closed bool
}

// newBacklog builds an empty backlog.
func newBacklog() *backlog {
	b := &backlog{}
	b.room.L = &b.mu
	return b
}

// take waits until a frame of n bytes may be held and holds it, reporting
// false once the backlog is closed. One frame is always held, whatever its
// size, so a frame as large as the bound is still delivered.
func (b *backlog) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for !b.closed && b.held > 0 && b.held+n > maxFrameBytes {
		b.room.Wait()
	}
	if b.closed {
		return false
	}
	b.held += n
	return true
}

// release lets go of a frame of n bytes that was received.
func (b *backlog) release(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= n
	b.room.Broadcast()
}

// close releases a reader waiting for room: the subprocess was given up, and
// nothing will be received any more.
func (b *backlog) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.room.Broadcast()
}

// tailBuffer keeps the last limit bytes written to it. It is safe for the
// concurrent writes of the exec stderr copier and the reads of closedError.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

// Write keeps the tail of everything written so far. It always reports the
// whole of p as written: the copier it serves stops at a short write, and the
// subprocess would then block on a pipe nobody drains.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	if overflow := len(b.buf) + len(p) - b.limit; overflow > 0 {
		b.buf = append(b.buf[:0], b.buf[overflow:]...)
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns what is kept.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Compile-time assertions that *StdioTransport satisfies Transport and the
// optional hook it takes part in.
var (
	_ Transport         = (*StdioTransport)(nil)
	_ CancellationAware = (*StdioTransport)(nil)
)

// NotifiesCancellation reports that a request abandoned over stdio is withdrawn
// by a notifications/cancelled frame, whatever the revision: the subprocess is
// one server for every request, and the specification has the client cancel a
// request rather than end the process that holds all the others.
func (t *StdioTransport) NotifiesCancellation(ProtocolVersion) bool { return true }

// NewStdioTransport builds a stdio transport that will run command with args.
func NewStdioTransport(command string, args ...string) *StdioTransport {
	return &StdioTransport{
		command: command,
		args:    append([]string(nil), args...),
		timeout: defaultTimeout,
	}
}

// SetTimeout sets the receive timeout applied when the context has no deadline.
func (t *StdioTransport) SetTimeout(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timeout = d
}

// Timeout returns the receive timeout, which the client also holds a whole
// exchange to.
func (t *StdioTransport) Timeout() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timeout
}

// Recipe returns the transport's serializable description.
func (t *StdioTransport) Recipe() Recipe {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Recipe{Driver: "stdio", Command: t.command, Args: append([]string(nil), t.args...), Timeout: t.timeout}
}

// Connect spawns the subprocess and starts the stdout reader. It is idempotent.
func (t *StdioTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil {
		return nil
	}

	cmd := exec.Command(t.command, t.args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return wrapError(err, "unable to open subprocess stdin")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return wrapError(err, "unable to open subprocess stdout")
	}
	t.stderr = &tailBuffer{limit: maxStderrBytes}
	cmd.Stderr = t.stderr
	cmd.WaitDelay = pipeGrace

	if err := cmd.Start(); err != nil {
		return wrapError(err, "failed to start process ["+t.command+"]; make sure the command exists")
	}

	t.cmd = cmd
	t.stdin = stdin
	t.lines = make(chan string, 16)
	t.readErr = make(chan error, 1)
	t.unread = newBacklog()
	t.done = make(chan struct{})
	go t.readLoop(stdout, t.lines, t.readErr, t.unread, t.done)
	return nil
}

// readLoop reads newline-delimited frames from stdout until the stream ends,
// forwarding each frame and finally the terminating error.
//
// It ends with the stream and with nothing else, so it cannot outlive the
// subprocess: once done is closed nobody receives a frame any more, and what
// the subprocess still writes is read and dropped. The reading goes on because
// a server asked to shut down may still be writing, and one blocked on a pipe
// nobody drains never gets to exit.
//
// A frame larger than maxFrameBytes ends the delivery as the end of the stream
// does: the stream cannot be put back in step past a frame that was not read to
// its end. What is read and not yet received is bounded by unread: a frame is
// handed on only once there is room for it, and the reading waits until then.
func (t *StdioTransport) readLoop(stdout io.Reader, lines chan<- string, readErr chan<- error, unread *backlog, done <-chan struct{}) {
	reader := bufio.NewReader(stdout)
	for {
		line, err := readFrame(reader, maxFrameBytes)
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			if !unread.take(len(trimmed)) {
				_, _ = io.Copy(io.Discard, reader)
				return
			}
			select {
			case lines <- trimmed:
			case <-done:
				_, _ = io.Copy(io.Discard, reader)
				return
			}
		}
		if err != nil {
			readErr <- err
			if errors.Is(err, errFrameTooLarge) {
				// The stream has not ended, only the reading of it as frames.
				_, _ = io.Copy(io.Discard, reader)
			}
			return
		}
	}
}

// errFrameTooLarge ends the reading of a subprocess that sent a frame larger
// than the transport takes.
var errFrameTooLarge = errors.New("a frame exceeded the size this client reads")

// readFrame reads one line, giving up with errFrameTooLarge once it has read
// more than limit bytes of it. What was read of an oversized line is dropped,
// so nothing of it is taken for a frame.
func readFrame(reader *bufio.Reader, limit int) (string, error) {
	var frame []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(frame)+len(chunk) > limit {
			return "", errFrameTooLarge
		}
		frame = append(frame, chunk...)
		if err != bufio.ErrBufferFull {
			return string(frame), err
		}
	}
}

// Send writes a single frame followed by a newline to the subprocess stdin.
//
// The write is bounded by the timeout, by a deadline of ctx that comes sooner,
// and by ctx ending: a server that has stopped reading its input leaves the pipe
// full, and a write into it would otherwise never return.
func (t *StdioTransport) Send(ctx context.Context, message string) error {
	t.mu.Lock()
	stdin, timeout := t.stdin, t.timeout
	t.mu.Unlock()
	if stdin == nil {
		return newError("transport is not connected")
	}
	if bounded, ok := stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		// The timeout is the bound, and a deadline of ctx only ever shortens
		// it. The context an exchange runs under states the maximum of the
		// exchange as its deadline, which is many timeouts away, and a write
		// held to that would hold the exchange gate with it.
		deadline := time.Now().Add(timeout)
		if stated, ok := ctx.Deadline(); ok && stated.Before(deadline) {
			deadline = stated
		}
		// A platform whose pipes take no deadline leaves the write unbounded,
		// as it was.
		_ = bounded.SetWriteDeadline(deadline)
		// A write blocked on a full pipe does not watch the context, so a
		// context that ends first ends the write through its deadline.
		stop := context.AfterFunc(ctx, func() { _ = bounded.SetWriteDeadline(time.Now()) })
		defer stop()
	}
	if _, err := io.WriteString(stdin, message+"\n"); err != nil {
		// A pipe that will not take the frame is the subprocess having gone
		// away or stopped reading, not a server refusing the request, so it is
		// reported as the channel failure it is and the subprocess is torn
		// down: part of a frame may have been written, and the next Connect has
		// to start a new server rather than adopt this one.
		_ = t.Disconnect()
		return NewTransportError("unable to write to subprocess ["+t.command+"]", err)
	}
	return nil
}

// Receive returns the next frame, blocking until one arrives or the
// context/timeout elapses or the subprocess closes its output.
func (t *StdioTransport) Receive(ctx context.Context) (string, error) {
	t.mu.Lock()
	lines, readErr, unread, done, timeout := t.lines, t.readErr, t.unread, t.done, t.timeout
	t.mu.Unlock()
	if lines == nil {
		return "", newError("transport is not connected")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case line := <-lines:
		unread.release(len(line))
		return line, nil
	case err := <-readErr:
		// Drain any line buffered alongside the terminating error.
		select {
		case line := <-lines:
			unread.release(len(line))
			return line, nil
		default:
		}
		select {
		case <-done:
			// The output ended because the subprocess was given up, which is
			// not the server closing it on a request.
			return "", newError("transport is not connected")
		default:
		}
		return "", t.closedError(err)
	case <-done:
		// The subprocess was given up while this wait was in flight.
		return "", newError("transport is not connected")
	case <-ctx.Done():
		return "", t.abandonedError(ctx.Err())
	case <-timer.C:
		return "", timeoutError(nil)
	}
}

// abandonedError reports a wait the context ended. A deadline that elapsed is a
// timeout like any other; a cancelled context is the caller withdrawing the
// request, which is neither a timeout nor a failure of the channel, so it is a
// plain client error that no other handshake would do better with.
//
// The subprocess is left running either way. It serves every request of the
// connection, and the one that was abandoned is withdrawn by the client with a
// notifications/cancelled frame: whatever the server still sends for it carries
// the id of a request nobody is waiting for, and is passed over by whoever
// reads next.
func (t *StdioTransport) abandonedError(cause error) error {
	if errors.Is(cause, context.Canceled) {
		return wrapError(cause, "the wait for a response from subprocess ["+t.command+"] was cancelled")
	}
	return timeoutError(cause)
}

// timeoutError reports a wait that outlasted the timeout or the deadline of its
// context.
func timeoutError(cause error) error {
	return NewTimeoutError("timed out while waiting for server response", cause)
}

// closedError annotates an early stream close with any captured stderr. The
// subprocess is torn down first, because a server whose output has ended is
// gone and Connect is idempotent: left in place, the exec state would have the
// next connect adopt the dead process instead of starting a new one.
//
// It is a transport failure. A subprocess that ends on a request it does not
// know is a server that predates that request answering it the only way it can,
// and classifying it as such is what lets the connection probe start the server
// again and negotiate with the handshake it does speak.
func (t *StdioTransport) closedError(err error) error {
	if err == io.EOF {
		err = nil
	}
	t.mu.Lock()
	captured := t.stderr
	t.mu.Unlock()
	// The subprocess is reaped before its standard error is read, so what it
	// said last is in the report however the two streams raced each other.
	_ = t.Disconnect()

	msg := "subprocess [" + t.command + "] closed its output before sending a complete response"
	if errors.Is(err, errFrameTooLarge) {
		msg = "subprocess [" + t.command + "] sent a frame this client does not read to its end"
	}
	if captured != nil {
		// What the server said is quoted escaped, as every text a server sent
		// is (see quoted), and it is the tail of it that is kept: the last
		// thing it said is what explains why it ended.
		if stderr := escaped(strings.TrimSpace(captured.String())); stderr != "" {
			if len(stderr) > maxStderrBytes {
				stderr = stderr[len(stderr)-maxStderrBytes:]
			}
			msg += "; stderr: " + stderr
		}
	}
	return NewTransportError(msg, err)
}

// Disconnect stops the subprocess the way the specification has a client stop
// a server it started: its input is closed, which is the request to shut down,
// and it is given shutdownGrace to exit by itself; a server that has not is
// asked to terminate and given as long again, and only one that still has not
// is killed. It is safe to call when not connected.
//
// It returns within a bound however the subprocess behaves. A launcher such as
// a shell or a package runner commonly leaves the server as a descendant
// holding the pipes it inherited, and waiting for those to close would wait for
// a process this transport never started: once the subprocess itself has
// exited its output is waited for no longer than pipeGrace.
func (t *StdioTransport) Disconnect() error {
	t.mu.Lock()
	cmd, stdin, unread, done := t.cmd, t.stdin, t.unread, t.done
	t.cmd, t.stdin, t.lines, t.readErr, t.unread, t.done = nil, nil, nil, nil, nil, nil
	t.mu.Unlock()

	if done != nil {
		close(done)
	}
	if unread != nil {
		unread.close()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()
	if exitedWithin(exited, shutdownGrace) {
		return nil
	}
	// A platform that cannot deliver the signal goes straight to the kill.
	if cmd.Process.Signal(syscall.SIGTERM) == nil && exitedWithin(exited, shutdownGrace) {
		return nil
	}
	_ = cmd.Process.Kill()
	<-exited
	return nil
}

// exitedWithin reports whether the subprocess was reaped within the grace.
func exitedWithin(exited <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
		return true
	case <-timer.C:
		return false
	}
}
