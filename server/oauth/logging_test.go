package oauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/router"
)

// recordingLogger captures what the endpoint logs so the "detail stays
// server-side" rule can be checked from both ends: absent from the response,
// present in the log.
type recordingLogger struct {
	mu   sync.Mutex
	msgs []string
	kvs  [][]any
}

func (l *recordingLogger) Debug(msg string, kvs ...any) {}
func (l *recordingLogger) Info(msg string, kvs ...any)  {}
func (l *recordingLogger) Warn(msg string, kvs ...any)  {}
func (l *recordingLogger) Fatal(msg string, kvs ...any) {}
func (l *recordingLogger) Error(msg string, kvs ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
	l.kvs = append(l.kvs, kvs)
}

func (l *recordingLogger) entries() ([]string, [][]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...), append([][]any(nil), l.kvs...)
}

func TestRegister_FailureIsLoggedNotReturned(t *testing.T) {
	tests := []struct {
		name    string
		store   *stubStore
		wantKV  string
		absent  string
		message string
	}{
		{
			name:    "store error",
			store:   &stubStore{failWith: errors.New("dial tcp 10.0.0.7:5432: connect: refused")},
			wantKV:  "dial tcp 10.0.0.7:5432: connect: refused",
			absent:  "10.0.0.7",
			message: "mcp oauth client registration failed",
		},
		{
			name:    "store returned no identifier",
			store:   &stubStore{useAnswer: true, answer: RegisteredClient{}},
			wantKV:  "the client store returned no client id",
			message: "mcp oauth client registration failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &recordingLogger{}
			r := router.NewV2()
			r.SetServices(&velapp.Services{Log: logger})
			Routes(r, openConfig(tt.store))

			w := httptest.NewRecorder()
			r.ServeHTTP(w, registrationRequest("/oauth/register", `{"redirect_uris":["https://app.example.test/cb"]}`))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			msgs, kvs := logger.entries()
			if len(msgs) != 1 || msgs[0] != tt.message {
				t.Fatalf("logged %v, want exactly [%q]", msgs, tt.message)
			}
			var detail string
			for i := 0; i+1 < len(kvs[0]); i += 2 {
				if kvs[0][i] == "error" {
					detail, _ = kvs[0][i+1].(string)
				}
			}
			if detail != tt.wantKV {
				t.Fatalf("logged error = %q, want %q", detail, tt.wantKV)
			}
			if tt.absent != "" && strings.Contains(w.Body.String(), tt.absent) {
				t.Fatalf("body %s leaked %q", w.Body.String(), tt.absent)
			}
		})
	}
}

// Without a wired logger the endpoint still answers rather than crashing on a
// nil service container, which is the shape a bare router has.
func TestRegister_FailureWithoutALoggerStillAnswers(t *testing.T) {
	w := register(t, openConfig(&stubStore{failWith: errors.New("boom")}),
		`{"redirect_uris":["https://app.example.test/cb"]}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
