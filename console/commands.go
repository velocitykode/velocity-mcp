package console

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"

	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/transport"
)

// ServerCommands returns the runtime MCP commands bound to srv: mcp:start (serve
// over stdio) and mcp:inspect (list the registered primitives). They need a live
// server, so a provider builds them from the server it serves (see
// provider.Provider.Commands). srv must be non-nil.
func ServerCommands(srv *server.Server) []chain.Command {
	return []chain.Command{
		startCommand{srv: srv},
		inspectCommand{srv: srv, out: os.Stdout},
	}
}

// startCommand serves the MCP server over stdio: a line-delimited JSON-RPC loop
// on stdin/stdout, the transport an MCP client launches as a subprocess. It runs
// until the client closes stdin (EOF) or the process is interrupted.
type startCommand struct {
	srv *server.Server
}

// Name implements chain.Command.
func (startCommand) Name() string { return "mcp:start" }

// Description implements chain.Command.
func (startCommand) Description() string {
	return "Serve the MCP server over stdio (JSON-RPC on stdin/stdout)"
}

// Handle runs the stdio transport until EOF or an interrupt signal. It writes
// nothing to stdout: stdout carries the JSON-RPC protocol stream, so any extra
// output would corrupt it (diagnostics belong on stderr or the app logger).
func (c startCommand) Handle(s *velapp.Services, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return transport.ServeStdio(ctx, c.srv)
}

// inspectCommand lists the server's registered tools, resources, resource
// templates, and prompts, for verifying what a server exposes without driving
// the protocol. Output goes to out (os.Stdout in production; a buffer in tests).
type inspectCommand struct {
	srv *server.Server
	out io.Writer
}

// Name implements chain.Command.
func (inspectCommand) Name() string { return "mcp:inspect" }

// Description implements chain.Command.
func (inspectCommand) Description() string {
	return "List the MCP server's registered tools, resources, and prompts"
}

// Handle writes the server's primitive inventory to the command's writer.
func (c inspectCommand) Handle(s *velapp.Services, args []string) error {
	w := c.out
	if w == nil {
		w = os.Stdout
	}
	return writeInventory(w, c.srv)
}

// writeInventory renders the server identity and its registered primitives to w.
// Each section reports its count and lists one primitive per line; an empty
// section is shown as "(none)" so the output is unambiguous about what is
// registered.
func writeInventory(w io.Writer, srv *server.Server) error {
	bw := &errWriter{w: w}

	bw.printf("%s %s\n", srv.Name(), srv.Version())
	if instr := srv.Instructions(); instr != "" {
		bw.printf("%s\n", instr)
	}

	bw.printf("\nTools (%d):\n", len(srv.Tools()))
	if len(srv.Tools()) == 0 {
		bw.printf("  (none)\n")
	}
	for _, t := range srv.Tools() {
		bw.printf("  %s - %s\n", t.Name(), t.Description())
	}

	bw.printf("\nResources (%d):\n", len(srv.Resources()))
	if len(srv.Resources()) == 0 {
		bw.printf("  (none)\n")
	}
	for _, r := range srv.Resources() {
		bw.printf("  %s [%s]\n", r.Name(), r.URI())
	}

	bw.printf("\nResource templates (%d):\n", len(srv.ResourceTemplates()))
	if len(srv.ResourceTemplates()) == 0 {
		bw.printf("  (none)\n")
	}
	for _, t := range srv.ResourceTemplates() {
		bw.printf("  %s [%s]\n", t.Name(), t.URITemplate())
	}

	bw.printf("\nPrompts (%d):\n", len(srv.Prompts()))
	if len(srv.Prompts()) == 0 {
		bw.printf("  (none)\n")
	}
	for _, p := range srv.Prompts() {
		bw.printf("  %s - %s\n", p.Name(), p.Description())
	}

	return bw.err
}

// errWriter wraps an io.Writer so a sequence of formatted writes can be issued
// without checking each one; the first write error is retained and returned.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
