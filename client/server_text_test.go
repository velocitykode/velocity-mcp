package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers what a client error quotes of the text a server sent. A
// server names things, in the frames it sends and on its standard error, and an
// error that quoted them verbatim would hand the server whatever renders the
// error: a control character in the quoted text is acted on by a terminal, and
// a line break by a log reader. What the server sent is quoted with its control
// characters escaped and its length bounded.

// controlText is a name a server might send: a control character, a line
// break, and a word that has to survive the quoting.
const controlText = "\x1b" + "x\nword"

// hasControl reports whether a message carries a raw control character.
func hasControl(message string) bool {
	return strings.ContainsFunc(message, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func TestServerTextInAClientErrorIsEscapedAndBounded(t *testing.T) {
	escaped := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("the exchange succeeded, want an error quoting what the server sent")
		}
		if hasControl(err.Error()) {
			t.Fatalf("the error carries a raw control character: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "word") {
			t.Fatalf("the error lost what the server sent: %q", err.Error())
		}
	}
	method, _ := json.Marshal(controlText)

	t.Run("the standard error of a stdio server", func(t *testing.T) {
		script := `printf '%s' '` + "\x1b" + `x' >&2; echo word >&2; IFS= read -r line; exit 3`
		c := New(NewStdioTransport("/bin/sh", "-c", script), testClientInfo())
		defer c.Disconnect()
		escaped(t, c.Connect(context.Background()))
	})

	t.Run("the method of a frame carrying no valid id", func(t *testing.T) {
		f := newFakeTransport()
		f.framesBefore["prompts/list"] = []string{`{"jsonrpc":"2.0","id":null,"method":` + string(method) + `}`}
		c := New(f, testClientInfo())
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		_, err := c.Prompts(context.Background())
		escaped(t, err)
	})

	t.Run("the method of a request the revision forbids", func(t *testing.T) {
		f := newFakeTransport()
		f.on("server/discover", discoverHandler(LatestProtocolVersion))
		f.framesBefore["prompts/list"] = []string{serverRequestFrame("7", strings.Trim(string(method), `"`))}
		c := New(f, testClientInfo())
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		_, err := c.Prompts(context.Background())
		escaped(t, err)
	})

	t.Run("the versions a server supports", func(t *testing.T) {
		f := newFakeTransport()
		f.on("server/discover", discoverHandler(controlText))
		c := New(f, testClientInfo())
		escaped(t, c.Connect(context.Background()))
	})

	t.Run("the version a server chose", func(t *testing.T) {
		f := newFakeTransport()
		f.on("initialize", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
			resp, _ := jsonrpc.NewResult(id, map[string]any{
				"protocolVersion": controlText,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0.0"},
			})
			return resp
		})
		c := New(f, testClientInfo())
		escaped(t, c.Connect(context.Background()))
	})

	t.Run("a name longer than any error should quote", func(t *testing.T) {
		f := newFakeTransport()
		f.framesBefore["prompts/list"] = []string{`{"jsonrpc":"2.0","id":null,"method":"` + strings.Repeat("m", 64<<10) + `"}`}
		c := New(f, testClientInfo())
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("connect: %v", err)
		}
		_, err := c.Prompts(context.Background())
		if err == nil {
			t.Fatal("the exchange succeeded, want an error")
		}
		if got := len(err.Error()); got > 1<<10 {
			t.Fatalf("the error is %d bytes long, want it bounded", got)
		}
	})
}
