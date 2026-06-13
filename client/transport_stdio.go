package client

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

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
	stderr  *syncBuffer
	lines   chan string
	readErr chan error
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes performed by the
// exec stderr copier and the reads performed by closedError.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Compile-time assertion that *StdioTransport satisfies Transport.
var _ Transport = (*StdioTransport)(nil)

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
	t.stderr = &syncBuffer{}
	cmd.Stderr = t.stderr

	if err := cmd.Start(); err != nil {
		return wrapError(err, "failed to start process ["+t.command+"]; make sure the command exists")
	}

	t.cmd = cmd
	t.stdin = stdin
	t.lines = make(chan string, 16)
	t.readErr = make(chan error, 1)
	go t.readLoop(stdout, t.lines, t.readErr)
	return nil
}

// readLoop reads newline-delimited frames from stdout until the stream ends,
// forwarding each frame and finally the terminating error.
func (t *StdioTransport) readLoop(stdout io.Reader, lines chan<- string, readErr chan<- error) {
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			lines <- trimmed
		}
		if err != nil {
			readErr <- err
			return
		}
	}
}

// Send writes a single frame followed by a newline to the subprocess stdin.
func (t *StdioTransport) Send(ctx context.Context, message string) error {
	t.mu.Lock()
	stdin := t.stdin
	t.mu.Unlock()
	if stdin == nil {
		return newError("transport is not connected")
	}
	if _, err := io.WriteString(stdin, message+"\n"); err != nil {
		return wrapError(err, "unable to write to subprocess")
	}
	return nil
}

// Receive returns the next frame, blocking until one arrives or the
// context/timeout elapses or the subprocess closes its output.
func (t *StdioTransport) Receive(ctx context.Context) (string, error) {
	t.mu.Lock()
	lines, readErr, timeout := t.lines, t.readErr, t.timeout
	t.mu.Unlock()
	if lines == nil {
		return "", newError("transport is not connected")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case line := <-lines:
		return line, nil
	case err := <-readErr:
		// Drain any line buffered alongside the terminating error.
		select {
		case line := <-lines:
			return line, nil
		default:
		}
		return "", t.closedError(err)
	case <-ctx.Done():
		return "", wrapError(ctx.Err(), "timed out while waiting for server response")
	case <-timer.C:
		return "", newError("timed out while waiting for server response")
	}
}

// closedError annotates an early stream close with any captured stderr.
func (t *StdioTransport) closedError(err error) error {
	if err == io.EOF {
		err = nil
	}
	t.mu.Lock()
	stderr := ""
	if t.stderr != nil {
		stderr = strings.TrimSpace(t.stderr.String())
	}
	t.mu.Unlock()
	msg := "subprocess [" + t.command + "] closed its output before sending a complete response"
	if stderr != "" {
		msg += "; stderr: " + stderr
	}
	if err != nil {
		return wrapError(err, msg)
	}
	return newError(msg)
}

// Disconnect closes stdin and stops the subprocess. It is safe to call when not
// connected.
func (t *StdioTransport) Disconnect() error {
	t.mu.Lock()
	cmd, stdin := t.cmd, t.stdin
	t.cmd, t.stdin, t.lines, t.readErr = nil, nil, nil, nil
	t.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return nil
}
