package oauth

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDynamicRegistrationHappensOncePerClient(t *testing.T) {
	as := newMetadataServer(t, nil)
	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, as.srv.URL, "", "")

	first := authorizeQuery(t, c).Get("client_id")
	second := authorizeQuery(t, c).Get("client_id")

	if first != "dyn-client" || second != first {
		t.Fatalf("client ids = %q and %q, want a single reused registration", first, second)
	}
	if n := as.registered.Load(); n != 1 {
		t.Fatalf("registration requests = %d, want 1; every authorization start registered again", n)
	}
	if n := as.discoveries.Load(); n != 1 {
		t.Fatalf("metadata fetches = %d, want 1", n)
	}
}

func TestConcurrentAuthorizationURLRegistersOnce(t *testing.T) {
	as := newMetadataServer(t, nil)
	c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, as.srv.URL, "", "")

	const flows = 8
	var wg sync.WaitGroup
	states := make(chan string, flows)
	errs := make(chan error, flows)

	wg.Add(flows)
	for range flows {
		go func() {
			defer wg.Done()
			_, pending, err := c.AuthorizationURL(context.Background(), "")
			if err != nil {
				errs <- err
				return
			}
			if pending.ClientID != "dyn-client" {
				errs <- newError("client id = %q, want dyn-client", pending.ClientID)
				return
			}
			states <- pending.State
		}()
	}
	wg.Wait()
	close(errs)
	close(states)

	for err := range errs {
		t.Fatalf("concurrent authorization: %v", err)
	}
	if n := as.registered.Load(); n != 1 {
		t.Fatalf("registration requests = %d, want 1", n)
	}

	// Every concurrent flow still gets its own anti-CSRF state.
	seen := map[string]bool{}
	for state := range states {
		if state == "" || seen[state] {
			t.Fatalf("duplicate or empty state %q across concurrent flows", state)
		}
		seen[state] = true
	}
	if len(seen) != flows {
		t.Fatalf("states = %d, want %d", len(seen), flows)
	}
}

func TestDynamicRegistrationPayload(t *testing.T) {
	as := newMetadataServer(t, nil)
	c := NewClient(Config{RedirectURI: "https://app.example.com/callback", Scope: "mcp:use custom"}, as.srv.URL, "", "")
	authorizeQuery(t, c)

	body := <-as.registerBody
	if got := body["redirect_uris"]; !equalStrings(got, []string{"https://app.example.com/callback"}) {
		t.Fatalf("redirect_uris = %v", got)
	}
	if got := body["grant_types"]; !equalStrings(got, []string{"authorization_code", "refresh_token"}) {
		t.Fatalf("grant_types = %v", got)
	}
	if got := body["response_types"]; !equalStrings(got, []string{"code"}) {
		t.Fatalf("response_types = %v", got)
	}
	if body["application_type"] != "web" {
		t.Fatalf("application_type = %v, want web", body["application_type"])
	}
	if body["scope"] != "mcp:use custom" {
		t.Fatalf("scope = %v", body["scope"])
	}
	if body["token_endpoint_auth_method"] != authMethodPost {
		t.Fatalf("token_endpoint_auth_method = %v, want %s", body["token_endpoint_auth_method"], authMethodPost)
	}
}

func TestAuthorizationURLWithoutAnyClientIdentitySource(t *testing.T) {
	// No configured client id, no usable metadata document, and a server that
	// does not offer registration: the flow cannot identify the client.
	as := newMetadataServer(t, map[string]any{"registration_endpoint": nil})
	c := NewClient(Config{
		RedirectURI:         "https://app.example.com/callback",
		ClientIDMetadataURL: "https://app.example.com/client-metadata.json",
	}, as.srv.URL, "", "")

	_, _, err := c.AuthorizationURL(context.Background(), "")
	if err == nil {
		t.Fatal("expected an error when no client identity can be resolved")
	}
	if !strings.Contains(err.Error(), "dynamic client registration") {
		t.Fatalf("error = %v, want it to name the missing registration support", err)
	}
}

// fixedRegistrationAS is a fake authorization server that answers every
// registration with the given response, weaving a sequence number into the
// issued client id so a reused record is distinguishable from a fresh one.
func fixedRegistrationAS(t *testing.T, response map[string]any) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	registrations := &atomic.Int64{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/authorize",
			"token_endpoint":                   srv.URL + "/token",
			"registration_endpoint":            srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		issued := maps.Clone(response)
		issued["client_id"] = fmt.Sprintf("dyn-client-%d", registrations.Add(1))
		writeJSON(w, issued)
	})
	return srv, registrations
}

func TestRegistrationExpiryGovernsReuse(t *testing.T) {
	// client_secret_expires_at (RFC 7591) is seconds since the epoch, with 0
	// meaning the issued secret never expires. It describes the secret, so it
	// only retires a record that actually carries one: a public client has
	// nothing that can expire, and a value that is not a positive number says
	// nothing at all.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour).Unix()
	future := now.Add(30 * time.Minute).Unix()

	tests := []struct {
		name              string
		response          map[string]any
		wantSecondID      string
		wantRegistrations int64
	}{
		{
			name:              "a secret that has expired retires the record",
			response:          map[string]any{"client_secret": "dyn-secret", "client_secret_expires_at": past},
			wantSecondID:      "dyn-client-2",
			wantRegistrations: 2,
		},
		{
			name:              "a secret that is still live is reused",
			response:          map[string]any{"client_secret": "dyn-secret", "client_secret_expires_at": future},
			wantSecondID:      "dyn-client-1",
			wantRegistrations: 1,
		},
		{
			name:              "an expiry without a secret is not an expiry",
			response:          map[string]any{"client_secret_expires_at": past},
			wantSecondID:      "dyn-client-1",
			wantRegistrations: 1,
		},
		{
			name:              "a zero expiry means the secret never expires",
			response:          map[string]any{"client_secret": "dyn-secret", "client_secret_expires_at": 0},
			wantSecondID:      "dyn-client-1",
			wantRegistrations: 1,
		},
		{
			name:              "no expiry field at all",
			response:          map[string]any{"client_secret": "dyn-secret"},
			wantSecondID:      "dyn-client-1",
			wantRegistrations: 1,
		},
		{
			name:              "a non-numeric expiry is ignored",
			response:          map[string]any{"client_secret": "dyn-secret", "client_secret_expires_at": "tomorrow"},
			wantSecondID:      "dyn-client-1",
			wantRegistrations: 1,
		},
		{
			name:              "a negative expiry is ignored",
			response:          map[string]any{"client_secret": "dyn-secret", "client_secret_expires_at": -1},
			wantSecondID:      "dyn-client-1",
			wantRegistrations: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, registrations := fixedRegistrationAS(t, tt.response)
			c := NewClient(Config{RedirectURI: "https://app.example.com/callback"}, srv.URL+"/mcp", "", "")
			// Both starts happen at the same instant, so the maximum age of a
			// registration cannot be what retires it here.
			c.now = func() time.Time { return now }

			if got := authorizeQuery(t, c).Get("client_id"); got != "dyn-client-1" {
				t.Fatalf("first client_id = %q, want dyn-client-1", got)
			}
			if got := authorizeQuery(t, c).Get("client_id"); got != tt.wantSecondID {
				t.Fatalf("second client_id = %q, want %q", got, tt.wantSecondID)
			}
			if got := registrations.Load(); got != tt.wantRegistrations {
				t.Fatalf("registration requests = %d, want %d", got, tt.wantRegistrations)
			}
		})
	}
}

// equalStrings compares a decoded JSON array against the expected strings.
func equalStrings(value any, want []string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	for i, item := range items {
		s, ok := item.(string)
		if !ok || s != want[i] {
			return false
		}
	}
	return true
}
