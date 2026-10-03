package transport

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// This file covers which session id reaches application code over HTTP. The
// accessor promises "the id of the session that issued the request", and the
// initialize handshake's security guidance (2025-11-25, session management)
// is to verify session ids rather than trust the header, so a value the client
// wrote into Mcp-Session-Id reaches a handler only when the server issued it.
// A 2026-07-28 request has no sessions and its header is ignored.

// sessionServer builds a server with a tool that reports the session id its
// request carries, and a dispatcher that records the tool events. Further
// options are applied after the tool.
func sessionServer(t *testing.T, opts ...server.Option) (*server.Server, func() []event.ToolCalled) {
	t.Helper()
	whoami := server.NewTool("whoami", "Report the session").
		WithSchema(func(s *schema.Object) {}).
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return server.Text("session=" + req.SessionID()), nil
		})
	srv := server.New("sessions", "1.0.0", append([]server.Option{server.WithTools(whoami)}, opts...)...)

	var mu sync.Mutex
	var called []event.ToolCalled
	srv.SetEventDispatcher(func(_ context.Context, e any) error {
		if tc, ok := e.(event.ToolCalled); ok {
			mu.Lock()
			called = append(called, tc)
			mu.Unlock()
		}
		return nil
	})
	return srv, func() []event.ToolCalled {
		mu.Lock()
		defer mu.Unlock()
		return append([]event.ToolCalled(nil), called...)
	}
}

// openSession initializes over HTTP and returns the session id the response
// header carries.
func openSession(t *testing.T, srv *server.Server) string {
	t.Helper()
	w := serveWith(t, srv, string(initializeRequest(1)))
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status = %d (body %s)", w.Code, w.Body.String())
	}
	sid := w.Header().Get(sessionHeader)
	if sid == "" {
		t.Fatal("initialize assigned no session id")
	}
	return sid
}

// sessionSeenBy drives the whoami tool with the given body and headers and
// returns the session id the handler observed.
func sessionSeenBy(t *testing.T, srv *server.Server, body string, headers ...string) string {
	t.Helper()
	w := serveWith(t, srv, body, headers...)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("whoami refused: %+v", resp.Error)
	}
	body = string(resp.Result)
	at := strings.Index(body, "session=")
	if at < 0 {
		t.Fatalf("result %s does not report the session", body)
	}
	rest := body[at+len("session="):]
	return rest[:strings.IndexByte(rest, '"')]
}

// withLastByteChanged returns s with its final byte replaced by a different
// one, whatever that byte was: a fixed replacement would leave one id in
// sixteen unaltered, since the tag is hex.
func withLastByteChanged(s string) string {
	b := []byte(s)
	last := len(b) - 1
	if b[last] == '0' {
		b[last] = '1'
	} else {
		b[last] = '0'
	}
	return string(b)
}

const legacyWhoami = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`

const modernWhoami = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{` +
	`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},` +
	`"name":"whoami","arguments":{}}}`

// TestOnlyAnIssuedSessionIDReachesTheHandler asserts the three outcomes a
// legacy request can have: the id the server issued is handed on, an id the
// client invented or altered is not, and a request with no header has no
// session. The tool events carry the same value the handler saw.
func TestOnlyAnIssuedSessionIDReachesTheHandler(t *testing.T) {
	srv, events := sessionServer(t)
	sid := openSession(t, srv)

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"the issued id", sid, sid},
		{"the issued id padded with whitespace", "  " + sid + "  ", sid},
		{"an invented id", "victim-session-0001", ""},
		{"the issued id with its tag altered", withLastByteChanged(sid), ""},
		{"the issued id without its tag", sid[:strings.LastIndexByte(sid, '.')], ""},
		{"no header", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(events())
			headers := []string{}
			if tt.header != "" {
				headers = append(headers, sessionHeader, tt.header)
			}
			if got := sessionSeenBy(t, srv, legacyWhoami, headers...); got != tt.want {
				t.Fatalf("handler saw session %q, want %q", got, tt.want)
			}
			called := events()
			if len(called) != before+1 {
				t.Fatalf("ToolCalled events = %d, want %d", len(called), before+1)
			}
			if called[len(called)-1].SessionID != tt.want {
				t.Fatalf("ToolCalled.SessionID = %q, want %q", called[len(called)-1].SessionID, tt.want)
			}
		})
	}
}

// TestAStatelessRequestIgnoresTheSessionHeader asserts a 2026-07-28 request,
// which has no sessions, is handled without one even when it carries the id a
// legacy initialize issued.
func TestAStatelessRequestIgnoresTheSessionHeader(t *testing.T) {
	srv, events := sessionServer(t)
	sid := openSession(t, srv)
	headers := []string{HeaderProtocolVersion, "2026-07-28", HeaderMethod, "tools/call", HeaderName, "whoami", sessionHeader, sid}
	if got := sessionSeenBy(t, srv, modernWhoami, headers...); got != "" {
		t.Fatalf("handler saw session %q on a stateless request, want none", got)
	}
	called := events()
	if len(called) != 1 || called[0].SessionID != "" {
		t.Fatalf("ToolCalled events = %+v, want one with no session", called)
	}
}

// TestAServerThatCannotVouchForSessionsIsHandedNone asserts a server offering
// no check over its ids is never handed one: nothing can say the id was issued.
func TestAServerThatCannotVouchForSessionsIsHandedNone(t *testing.T) {
	stub := &stubServer{fn: func(context.Context, []byte, string) server.HandleResult {
		return server.HandleResult{}
	}}
	c, _ := postContext(t, legacyWhoami, sessionHeader, "anything")
	if err := Handler(stub)(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if got := stub.lastSession(); got != "" {
		t.Fatalf("stub was handed session %q, want none", got)
	}
}

// TestIssuedSessionIDSurvivesTheWireRoundTrip asserts the id as the response
// header carries it is the id the server vouches for, so a client echoing the
// header verbatim is recognised.
func TestIssuedSessionIDSurvivesTheWireRoundTrip(t *testing.T) {
	srv, _ := sessionServer(t)
	sid := openSession(t, srv)
	if !srv.IssuedSessionID(sid) {
		t.Fatalf("the server does not vouch for the id it sent: %q", sid)
	}
}

// TestAStatelessInitializeIsNotFoundAndMintsNoSession asserts an initialize
// declaring 2026-07-28 in its _meta is answered 404 and -32601 over HTTP, with
// no Mcp-Session-Id header: the revision has no sessions, so none is minted or
// echoed.
func TestAStatelessInitializeIsNotFoundAndMintsNoSession(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},` +
		`"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	srv, _ := sessionServer(t)
	w := serveWith(t, srv, body, HeaderProtocolVersion, "2026-07-28", HeaderMethod, "initialize")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("error = %+v, want code -32601", resp.Error)
	}
	if sid := w.Header().Get(sessionHeader); sid != "" {
		t.Fatalf("%s = %q on a stateless-era request, want none", sessionHeader, sid)
	}
}

// TestASessionOpenedOnOneReplicaIsHonouredOnAnother asserts what a deployment
// of several instances needs from the transport: the id one replica issued
// reaches the handler on another replica exactly when the two were given the
// same session key. Without a shared key the second replica cannot vouch for
// the id and the handler sees no session.
func TestASessionOpenedOnOneReplicaIsHonouredOnAnother(t *testing.T) {
	shared := bytes.Repeat([]byte{0x5a}, 32)
	other := bytes.Repeat([]byte{0xa5}, 32)
	tests := []struct {
		name       string
		keyA, keyB []byte
		honoured   bool
	}{
		{"both replicas share a key", shared, shared, true},
		{"the replicas hold different keys", shared, other, false},
		{"only the issuing replica has a key", shared, nil, false},
		{"neither replica has a key", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := sessionServer(t, server.WithSessionKey(tt.keyA))
			b, events := sessionServer(t, server.WithSessionKey(tt.keyB))
			sid := openSession(t, a)
			want := ""
			if tt.honoured {
				want = sid
			}
			if got := sessionSeenBy(t, b, legacyWhoami, sessionHeader, sid); got != want {
				t.Fatalf("handler on the second replica saw session %q, want %q", got, want)
			}
			if called := events(); len(called) != 1 || called[0].SessionID != want {
				t.Fatalf("ToolCalled events = %+v, want one naming session %q", called, want)
			}
			if got := sessionSeenBy(t, b, legacyWhoami, sessionHeader, withLastByteChanged(sid)); got != "" {
				t.Fatalf("handler saw an altered id as session %q", got)
			}
		})
	}
}
