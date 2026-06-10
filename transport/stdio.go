package transport

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"

	"github.com/velocitykode/velocity/async"
)

// maxLineBytes caps a single inbound JSON-RPC line so a hostile or buggy peer
// cannot drive unbounded memory growth on the stdio transport. The MCP spec
// places no hard size on a message; this matches a generous-but-bounded limit
// for line-delimited framing.
const maxLineBytes = 4 << 20 // 4 MiB

// Stdio is a line-delimited JSON-RPC transport over a reader/writer pair,
// mirroring laravel/mcp's StdioTransport. Each inbound line is one JSON-RPC
// message; each outbound message is written as one line terminated by '\n'. The
// reader and writer are parameterized so tests can drive the transport without
// real process stdio; ServeStdio wires them to os.Stdin/os.Stdout.
//
// A single Stdio drives one session: the session id assigned by an initialize
// response is retained and supplied to subsequent messages, matching the
// one-process-one-session model of stdio MCP servers.
type Stdio struct {
	srv MCPServer
	in  io.Reader
	out io.Writer

	// mu guards out (Send may be called concurrently with the loop's own
	// writes) and sessionID.
	mu        sync.Mutex
	sessionID string
}

// NewStdio constructs a Stdio transport for srv reading from in and writing to
// out. Passing a nil reader or writer falls back to os.Stdin / os.Stdout so the
// zero-configuration case "just works".
func NewStdio(srv MCPServer, in io.Reader, out io.Writer) *Stdio {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	return &Stdio{srv: srv, in: in, out: out}
}

// ServeStdio runs an MCP server over stdin/stdout as a blocking, line-delimited
// JSON-RPC loop. It is the entry point a console command (mcp:start) calls. The
// loop runs until ctx is cancelled or stdin reaches EOF, then returns. It never
// panics and returns a non-nil error only on an unrecoverable write failure.
func ServeStdio(ctx context.Context, srv MCPServer) error {
	return NewStdio(srv, os.Stdin, os.Stdout).Run(ctx)
}

// Run drives the read/handle/write loop until ctx is cancelled or the reader
// reaches EOF. Reads happen in a helper goroutine so a cancelled context stops
// the loop promptly even while blocked on a read. Run returns nil on a clean
// stop (EOF or cancellation) and a non-nil error only on a write failure.
func (t *Stdio) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	lines, errc := t.readLines(ctx)

	for {
		select {
		case <-ctx.Done():
			// Drain the reader goroutine so it can observe the cancellation and
			// exit; it is unblocked by the deferred close in readLines once the
			// reader returns. We simply stop consuming.
			return nil
		case <-errc:
			// The reader goroutine ended: EOF or a read error. Either way the
			// stream is done. A read error (non-EOF) ends the session but is not
			// surfaced as a failure to the caller, matching laravel/mcp's
			// StdioTransport, which simply loops until feof(STDIN).
			return nil
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if err := t.dispatch(ctx, line); err != nil {
				return err
			}
		}
	}
}

// dispatch handles one inbound line and writes any reply. A blank line is
// ignored. The session id assigned by an initialize response is retained for
// subsequent messages.
func (t *Stdio) dispatch(ctx context.Context, line []byte) error {
	if len(line) == 0 {
		return nil
	}

	res := t.srv.Handle(ctx, line, t.currentSessionID())
	if res.SessionID != "" {
		t.setSessionID(res.SessionID)
	}
	if !res.HasResponse || res.Response == nil {
		return nil
	}

	msg, err := encodeResponse(res.Response)
	if err != nil {
		// A response that cannot be marshalled is a server-side defect; we drop
		// it rather than crash the loop. There is no client id context to reply
		// to safely, so we simply skip the frame.
		return nil
	}
	return t.Send(ctx, msg)
}

// Send writes a single message frame followed by a newline, mirroring
// laravel/mcp's StdioTransport::send (fwrite(STDOUT, $message.PHP_EOL)). It is
// safe for concurrent use; writes are serialized by the transport mutex.
func (t *Stdio) Send(ctx context.Context, msg []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.out.Write(msg); err != nil {
		return err
	}
	_, err := t.out.Write([]byte{'\n'})
	return err
}

// SessionID returns the current session id (empty until an initialize response
// assigns one).
func (t *Stdio) SessionID() string { return t.currentSessionID() }

func (t *Stdio) currentSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *Stdio) setSessionID(id string) {
	t.mu.Lock()
	t.sessionID = id
	t.mu.Unlock()
}

// readLines scans the reader into a channel of complete lines (with the line
// terminator stripped). The returned error channel reports the terminating
// condition (io.EOF on a clean end). Both channels are closed when the reader
// goroutine exits, which it does on EOF, a read error, or ctx cancellation.
func (t *Stdio) readLines(ctx context.Context) (<-chan []byte, <-chan error) {
	lines := make(chan []byte)
	errc := make(chan error, 1)

	// async.Go runs the scan loop in a panic-recovered goroutine: a panic in the
	// scanner or a downstream send is contained rather than crashing the process
	// (the framework-max + raw-goroutine policy requires velocity's async helper
	// over a bare `go`). The closure captures ctx so it still observes
	// cancellation via the select on the channel send below.
	async.Go(func() {
		defer close(lines)
		defer close(errc)

		scanner := bufio.NewScanner(t.in)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

		for scanner.Scan() {
			// Copy the token: bufio.Scanner reuses its buffer between scans, so
			// the slice handed downstream must own its bytes.
			tok := scanner.Bytes()
			line := make([]byte, len(tok))
			copy(line, tok)

			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errc <- err
			return
		}
		errc <- io.EOF
	})

	return lines, errc
}
