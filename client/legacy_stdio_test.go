package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers the handshake against a server that predates the discovery
// request, over the transport such a server is actually reached through. The
// scripted transport can only report the failures it is told to report; a real
// subprocess reports the one the operating system gives it, which is the shape
// the fallback has to recognise.

// legacyServerEnv marks a process as the scripted legacy server below rather
// than the test binary.
const legacyServerEnv = "VELOCITY_MCP_TEST_LEGACY_STDIO_SERVER"

// legacyServerModeEnv selects how the scripted legacy server answers a request
// it does not know. Empty (the default) ends the process without a reply;
// "refuse" answers on protocol terms and then ends. Both are ways a server
// written before the discovery request answers it, and the second is the one
// that leaves the client holding a channel that looks alive and is not.
const legacyServerModeEnv = "VELOCITY_MCP_TEST_LEGACY_STDIO_MODE"

// TestMain runs the legacy server when this binary is started as one, so the
// stdio tests can spawn a real MCP server without a second binary to build.
func TestMain(m *testing.M) {
	if os.Getenv(legacyServerEnv) == "1" {
		os.Exit(runLegacyStdioServer())
	}
	os.Exit(m.Run())
}

// runLegacyStdioServer answers the initialize handshake over stdio and ends on
// any other request, which is how a server written before a request commonly
// answers it: the client sees the output close rather than a refusal on
// protocol terms. It returns the process exit status.
func runLegacyStdioServer() int {
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if decodeErr := json.Unmarshal([]byte(line), &frame); decodeErr != nil {
				fmt.Fprintf(os.Stderr, "unreadable frame: %v\n", decodeErr)
				return 2
			}
			switch frame.Method {
			case "initialize":
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,`+
					`"capabilities":{"tools":{}},"serverInfo":{"name":"Legacy Server","version":"1.0.0"}}}`+"\n",
					frame.ID, ProtocolV20251125)
			case "notifications/initialized":
			case "tools/list":
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"legacy-tool",`+
					`"description":"Runs on a server that predates discovery",`+
					`"inputSchema":{"type":"object","properties":{}}}]}}`+"\n", frame.ID)
			default:
				if os.Getenv(legacyServerModeEnv) == "refuse" {
					// Answered on protocol terms, then gone: the reply the
					// client reads is the last thing this process does.
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":`+
						`{"code":%d,"message":"Method not found."}}`+"\n", frame.ID, jsonrpc.CodeMethodNotFound)
					return 0
				}
				fmt.Fprintf(os.Stderr, "unknown method [%s]\n", frame.Method)
				return 1
			}
		}
		if err != nil {
			return 0
		}
	}
}

// legacyStdioTransport returns a transport that runs this test binary as the
// legacy server.
func legacyStdioTransport(t *testing.T) *StdioTransport {
	t.Helper()
	t.Setenv(legacyServerEnv, "1")
	transport := NewStdioTransport(os.Args[0])
	// The exchanges below all settle at once; the timeout is only a ceiling on
	// a machine slow to start the subprocess.
	transport.SetTimeout(30 * time.Second)
	return transport
}

// TestLegacySubprocessDyingOnTheProbeStillInitializes asserts the handshake
// against a server that ends on the discovery request: it is started again and
// negotiated with the handshake it does speak, and the connection it settles is
// usable. Such a server worked before the probe was sent at all, so the
// fallback has to hold for a channel that fails as well as for one that carries
// a refusal.
func TestLegacySubprocessDyingOnTheProbeStillInitializes(t *testing.T) {
	c := New(legacyStdioTransport(t), testClientInfo())
	defer c.Disconnect()

	ctx := context.Background()
	version, err := c.ProtocolVersion(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if version != ProtocolV20251125 {
		t.Fatalf("version = %q, want %q", version, ProtocolV20251125)
	}
	if info, err := c.ServerInfo(ctx); err != nil || info.Name != "Legacy Server" {
		t.Fatalf("serverInfo = %v (%v), want the server the fallback reached", info, err)
	}

	// The connection is the restarted subprocess, not the corpse of the one
	// that answered the probe by dying.
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "legacy-tool" {
		t.Fatalf("tools = %v, want the one the legacy server lists", tools)
	}
}

// TestLegacySubprocessRefusingTheProbeStillInitializes is the other half of the
// fallback, over a real subprocess: a server that answers the discovery request
// with a refusal and then ends. The refusal is an answer, not a failure of the
// channel, so nothing tears the channel down and the transport still holds a
// started process; the client only learns it is gone when it writes the
// initialize frame into a pipe nobody reads. The handshake has to recover from
// that by starting the server again, not report the dead channel.
func TestLegacySubprocessRefusingTheProbeStillInitializes(t *testing.T) {
	t.Setenv(legacyServerModeEnv, "refuse")
	c := New(legacyStdioTransport(t), testClientInfo())
	defer c.Disconnect()

	ctx := context.Background()
	version, err := c.ProtocolVersion(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if version != ProtocolV20251125 {
		t.Fatalf("version = %q, want %q", version, ProtocolV20251125)
	}

	// The connection is the restarted subprocess: it answers requests, which
	// the one that refused the probe could not.
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "legacy-tool" {
		t.Fatalf("tools = %v, want the one the legacy server lists", tools)
	}
	if info, err := c.ServerInfo(ctx); err != nil || info.Name != "Legacy Server" {
		t.Fatalf("serverInfo = %v (%v), want the server the fallback reached", info, err)
	}
}

// TestStdioClosedOutputIsAChannelFailure asserts what the transport reports
// when the subprocess ends mid-exchange, and that it starts over: the failure
// is a transport failure, and the next connect runs a new subprocess rather
// than adopting the dead one.
func TestStdioClosedOutputIsAChannelFailure(t *testing.T) {
	transport := legacyStdioTransport(t)
	ctx := context.Background()
	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer transport.Disconnect()

	if err := transport.Send(ctx, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err := transport.Receive(ctx)
	if err == nil {
		t.Fatal("a subprocess that ended mid-exchange was reported as a reply")
	}
	var transportErr *TransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("error = %v (%T), want a transport failure", err, err)
	}
	if !strings.Contains(err.Error(), "closed its output before sending a complete response") {
		t.Fatalf("error = %q", err.Error())
	}

	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if err := transport.Send(ctx, `{"jsonrpc":"2.0","id":2,"method":"initialize"}`); err != nil {
		t.Fatalf("send over the restarted subprocess: %v", err)
	}
	frame, err := transport.Receive(ctx)
	if err != nil {
		t.Fatalf("receive over the restarted subprocess: %v", err)
	}
	if !strings.Contains(frame, `"protocolVersion":"`+ProtocolV20251125+`"`) {
		t.Fatalf("frame = %q, want the initialize result of a new subprocess", frame)
	}
}
