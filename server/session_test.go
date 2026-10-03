package server_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity/contract"
	"github.com/velocitykode/velocity/webhook"

	"github.com/velocitykode/velocity-mcp/server"
)

// This file covers the one promise a session id carries: that a server can
// tell the ids it issued from everything else, with no record kept. The
// initialize handshake's security guidance (2025-11-25, session management) is
// that a server verify session ids rather than trust the header; the ids here
// are self-authenticating so a stateless transport can do that.

// initializeSession opens a session on s and returns the id the initialize
// result assigned.
func initializeSession(t *testing.T, s *server.Server) string {
	t.Helper()
	res := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`), "")
	if res.Response == nil || res.Response.Error != nil {
		t.Fatalf("initialize failed: %+v", res.Response)
	}
	if res.SessionID == "" {
		t.Fatal("initialize assigned no session id")
	}
	return res.SessionID
}

// TestIssuedSessionIDVouchesOnlyForTheServersOwnIDs asserts an id the server
// issued verifies and every alteration of it, every id of another server's
// making, and every value a client might invent does not.
func TestIssuedSessionIDVouchesOnlyForTheServersOwnIDs(t *testing.T) {
	s := server.New("demo", "1.0.0")
	other := server.New("demo", "1.0.0")
	id := initializeSession(t, s)
	foreign := initializeSession(t, other)

	if !s.IssuedSessionID(id) {
		t.Fatalf("the server does not vouch for the id it issued: %q", id)
	}
	if strings.IndexByte(id, '.') <= 0 {
		t.Fatalf("issued id %q carries no tag", id)
	}
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7E {
			t.Fatalf("issued id %q holds a byte a session id may not", id)
		}
	}

	raw := id[:strings.LastIndexByte(id, '.')]
	tag := id[strings.LastIndexByte(id, '.')+1:]
	flipped := func(s string, at int) string {
		b := []byte(s)
		if b[at] == 'a' {
			b[at] = 'b'
		} else {
			b[at] = 'a'
		}
		return string(b)
	}
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"the generated part without its tag", raw},
		{"the tag alone", tag},
		{"a separator and the tag", "." + tag},
		{"the tag with its last byte changed", raw + "." + flipped(tag, len(tag)-1)},
		{"the tag with its first byte changed", raw + "." + flipped(tag, 0)},
		{"the tag truncated", raw + "." + tag[:len(tag)-1]},
		{"the tag extended", id + "0"},
		{"the generated part changed", flipped(raw, 0) + "." + tag},
		{"another server's id", foreign},
		{"a client's invention", "victim-session-0001"},
		{"the id with a space appended", id + " "},
		{"the id with a non-ascii byte", id + "é"},
		{"a tag in upper case", raw + "." + strings.ToUpper(tag)},
		{"an id longer than any issued", strings.Repeat("a", 600) + "." + tag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if s.IssuedSessionID(tt.id) {
				t.Fatalf("the server vouched for %q", tt.id)
			}
		})
	}
	if other.IssuedSessionID(id) {
		t.Fatal("another server vouched for this server's id")
	}
}

// TestIssuedSessionIDFollowsTheGenerator asserts a custom generator's value is
// what the id starts with, that the generator's own separators do not confuse
// the tag, and that a generator producing a value a session id may not carry
// (one with a space) yields an id the server will not vouch for, which is the
// safe outcome for a misconfigured generator.
func TestIssuedSessionIDFollowsTheGenerator(t *testing.T) {
	tests := []struct {
		name      string
		generated string
		wantVouch bool
	}{
		{"a plain value", "sess-1", true},
		{"a value holding the separator", "tenant.42.session", true},
		{"a value ending in the separator", "sess.", true},
		{"a value with a space", "bad id", false},
		{"a value with a control byte", "bad\x01id", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0")
			s.SetSessionIDGenerator(func() string { return tt.generated })
			id := initializeSession(t, s)
			if !strings.HasPrefix(id, tt.generated+".") {
				t.Fatalf("id = %q, want the generator's value %q followed by the tag", id, tt.generated)
			}
			if got := s.IssuedSessionID(id); got != tt.wantVouch {
				t.Fatalf("IssuedSessionID(%q) = %v, want %v", id, got, tt.wantVouch)
			}
		})
	}
}

// TestIssuedSessionIDsDifferPerServer asserts two servers given the same
// generator still issue different ids: the tag is drawn from a per-server key,
// so an id minted by one deployment is not a session on another.
func TestIssuedSessionIDsDifferPerServer(t *testing.T) {
	a := server.New("demo", "1.0.0")
	b := server.New("demo", "1.0.0")
	for _, s := range []*server.Server{a, b} {
		s.SetSessionIDGenerator(func() string { return "sess-1" })
	}
	idA, idB := initializeSession(t, a), initializeSession(t, b)
	if idA == idB {
		t.Fatalf("two servers issued the same id %q for the same generated value", idA)
	}
	if a.IssuedSessionID(idB) || b.IssuedSessionID(idA) {
		t.Fatal("a server vouched for an id another server issued")
	}
}

// TestIssuedSessionIDIsSafeUnderConcurrentUse asserts verification and issuance
// can run from many goroutines at once, as they do on a shared HTTP server.
func TestIssuedSessionIDIsSafeUnderConcurrentUse(t *testing.T) {
	s := server.New("demo", "1.0.0")
	id := initializeSession(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !s.IssuedSessionID(id) {
				t.Error("an issued id stopped verifying")
			}
			if s.IssuedSessionID("victim-session-0001") {
				t.Error("an invented id verified")
			}
			if fresh := initializeSession(t, s); !s.IssuedSessionID(fresh) {
				t.Error("a freshly issued id does not verify")
			}
		}()
	}
	wg.Wait()
}

// TestSessionKeyDecidesWhichServersVouchForEachOther asserts the rule a
// deployment of several instances depends on: two servers vouch for each
// other's session ids exactly when they were given the same key. Servers given
// different keys, one key and none, or no key at all do not, and an empty key
// counts as none.
func TestSessionKeyDecidesWhichServersVouchForEachOther(t *testing.T) {
	shared := bytes.Repeat([]byte{0x5a}, 32)
	other := bytes.Repeat([]byte{0xa5}, 32)
	withKey := func(key []byte) []server.Option {
		return []server.Option{server.WithSessionKey(key)}
	}
	tests := []struct {
		name      string
		a, b      []server.Option
		wantVouch bool
	}{
		{"the same key", withKey(shared), withKey(shared), true},
		{"equal keys held in separate slices", withKey(shared), withKey(bytes.Clone(shared)), true},
		{"different keys", withKey(shared), withKey(other), false},
		{"a key and none", withKey(shared), nil, false},
		{"no key on either", nil, nil, false},
		{"an empty key on both", withKey([]byte{}), withKey([]byte{}), false},
		{"a nil key on both", withKey(nil), withKey(nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := server.New("demo", "1.0.0", tt.a...)
			b := server.New("demo", "1.0.0", tt.b...)
			idA, idB := initializeSession(t, a), initializeSession(t, b)
			if !a.IssuedSessionID(idA) || !b.IssuedSessionID(idB) {
				t.Fatal("a server does not vouch for the id it issued itself")
			}
			if got := b.IssuedSessionID(idA); got != tt.wantVouch {
				t.Fatalf("b vouching for a's id = %v, want %v", got, tt.wantVouch)
			}
			if got := a.IssuedSessionID(idB); got != tt.wantVouch {
				t.Fatalf("a vouching for b's id = %v, want %v", got, tt.wantVouch)
			}
			if a.IssuedSessionID("victim-session-0001") || b.IssuedSessionID(withTagByteChanged(idA)) {
				t.Fatal("a server vouched for an id nobody issued")
			}
		})
	}
}

// withTagByteChanged returns id with the last byte of its tag replaced by a
// different one, whatever that byte was.
func withTagByteChanged(id string) string {
	b := []byte(id)
	last := len(b) - 1
	if b[last] == 'a' {
		b[last] = 'b'
	} else {
		b[last] = 'a'
	}
	return string(b)
}

// TestSuppliedSessionKeyOutlivesARestart asserts a server built again with the
// same key, as one is after a restart, still vouches for the ids its
// predecessor issued, and that the server keeps its own copy of the key rather
// than the caller's slice.
func TestSuppliedSessionKeyOutlivesARestart(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	before := server.New("demo", "1.0.0", server.WithSessionKey(key))
	id := initializeSession(t, before)

	after := server.New("demo", "1.0.0", server.WithSessionKey(bytes.Clone(key)))
	if !after.IssuedSessionID(id) {
		t.Fatal("a server rebuilt with the same key does not vouch for an earlier id")
	}

	clear(key)
	if !before.IssuedSessionID(id) {
		t.Fatal("overwriting the caller's slice changed the server's key")
	}
	if fresh := initializeSession(t, before); !after.IssuedSessionID(fresh) {
		t.Fatal("ids issued after the caller's slice was overwritten no longer verify elsewhere")
	}
}

// TestSuppliedSessionKeyDoesNotSignDirectly asserts the tag is not the plain
// HMAC of the id under the supplied key: the server signs under a key derived
// from it, so a secret the application also uses elsewhere is safe to supply.
func TestSuppliedSessionKeyDoesNotSignDirectly(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	s := server.New("demo", "1.0.0", server.WithSessionKey(key))
	id := initializeSession(t, s)
	at := strings.LastIndexByte(id, '.')
	raw, tag := id[:at], id[at+1:]
	direct := hex.EncodeToString(webhook.HMACSHA256.Sign(key, []byte(raw)))
	if strings.HasPrefix(direct, tag) {
		t.Fatalf("tag %q is the HMAC of the id under the supplied key itself", tag)
	}
}

// errorLog records the error lines a server logs, with their arguments.
type errorLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *errorLog) Debug(string, ...any) {}
func (l *errorLog) Info(string, ...any)  {}
func (l *errorLog) Warn(string, ...any)  {}
func (l *errorLog) Fatal(string, ...any) {}
func (l *errorLog) With(kvs ...any) contract.Logger {
	return contract.BindFields(l, kvs...)
}
func (l *errorLog) Error(msg string, kvs ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(kvs...))
	l.mu.Unlock()
}

func (l *errorLog) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestSessionKeyShorterThanTheMinimumNeverSignsAnID asserts a key too short to
// resist a search is refused rather than used: an id shows a tag made under the
// key, so one issued id would let a client recover a one byte key in 256 tries
// and forge the rest. Two servers given a short key therefore do not vouch for
// each other (each keeps its own random key), no id either issues verifies
// under the short key, and the refusal is logged once as an error naming the
// length but not the key. An empty key is refused and reported like any other
// short one, as 0 bytes: using the option means a key was intended. A key of
// the minimum length or longer signs.
func TestSessionKeyShorterThanTheMinimumNeverSignsAnID(t *testing.T) {
	tests := []struct {
		length     int
		wantShared bool
		wantErrors int
	}{
		{0, false, 1},
		{1, false, 1},
		{31, false, 1},
		{32, true, 0},
		{64, true, 0},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d bytes", tt.length), func(t *testing.T) {
			key := bytes.Repeat([]byte{0x5a}, tt.length)
			logA, logB := &errorLog{}, &errorLog{}
			// The option is given before the logger on one server and after it
			// on the other: the report must not depend on the order.
			a := server.New("demo", "1.0.0", server.WithSessionKey(key), server.WithLogger(logA))
			b := server.New("demo", "1.0.0", server.WithLogger(logB), server.WithSessionKey(key))
			idA, idB := initializeSession(t, a), initializeSession(t, b)

			if !a.IssuedSessionID(idA) || !b.IssuedSessionID(idB) {
				t.Fatal("a server does not vouch for the id it issued itself")
			}
			if got := b.IssuedSessionID(idA); got != tt.wantShared {
				t.Fatalf("b vouching for a's id = %v, want %v", got, tt.wantShared)
			}
			if got := a.IssuedSessionID(idB); got != tt.wantShared {
				t.Fatalf("a vouching for b's id = %v, want %v", got, tt.wantShared)
			}

			// Nothing an attacker can compute from the short key reproduces the
			// tag: neither the key itself nor the key the server would have
			// derived from it signed the id.
			if !tt.wantShared {
				at := strings.LastIndexByte(idA, '.')
				raw, tag := idA[:at], idA[at+1:]
				derived := webhook.HMACSHA256.Sign(key, []byte("velocity-mcp session id key v1"))
				for name, candidate := range map[string][]byte{"the key itself": key, "the key derived from it": derived} {
					if strings.HasPrefix(hex.EncodeToString(webhook.HMACSHA256.Sign(candidate, []byte(raw))), tag) {
						t.Fatalf("the id was signed under %s", name)
					}
				}
			}

			for name, log := range map[string]*errorLog{"key before logger": logA, "logger before key": logB} {
				lines := log.errors()
				if len(lines) != tt.wantErrors {
					t.Fatalf("%s: errors logged = %q, want %d", name, lines, tt.wantErrors)
				}
				for _, line := range lines {
					if !strings.Contains(line, "session key") || !strings.Contains(line, fmt.Sprint(tt.length)) || !strings.Contains(line, "32") {
						t.Fatalf("%s: line %q does not name the refusal, the length given and the minimum", name, line)
					}
					if len(key) > 0 && strings.Contains(line, string(key)) {
						t.Fatalf("%s: line %q carries the key", name, line)
					}
				}
			}

			// The report is made once, at construction, not per request.
			initializeSession(t, a)
			a.IssuedSessionID(idB)
			if got := len(logA.errors()); got != tt.wantErrors {
				t.Fatalf("errors logged after further use = %d, want %d", got, tt.wantErrors)
			}
		})
	}
}

// TestRefusedSessionKeyLeavesAnAcceptedOneInPlace asserts a short key does not
// undo a key that was accepted, in either order, and is reported all the same.
func TestRefusedSessionKeyLeavesAnAcceptedOneInPlace(t *testing.T) {
	good := bytes.Repeat([]byte{0x5a}, 32)
	short := []byte{1}
	tests := []struct {
		name string
		opts []server.Option
	}{
		{"the short key second", []server.Option{server.WithSessionKey(good), server.WithSessionKey(short)}},
		{"the short key first", []server.Option{server.WithSessionKey(short), server.WithSessionKey(good)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &errorLog{}
			s := server.New("demo", "1.0.0", append(tt.opts, server.WithLogger(log))...)
			peer := server.New("demo", "1.0.0", server.WithSessionKey(good))
			if !peer.IssuedSessionID(initializeSession(t, s)) {
				t.Fatal("the accepted key no longer signs the server's ids")
			}
			if got := len(log.errors()); got != 1 {
				t.Fatalf("errors logged = %d, want 1", got)
			}
		})
	}
}
