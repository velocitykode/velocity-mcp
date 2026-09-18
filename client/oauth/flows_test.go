package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResourceMetadataMustDescribeTheRequestedResource(t *testing.T) {
	// A protected resource that claims to be a different resource is a mix-up
	// attempt: discovery must refuse it rather than follow its authorization
	// server.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              "https://someone-else.example.com/mcp",
			"authorization_servers": []string{srv.URL},
		})
	})

	_, err := NewDiscovery().Discover(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "did not match the expected resource") {
		t.Fatalf("error = %v, want a resource mismatch refusal", err)
	}
}

func TestResourceFragmentIsStrippedFromTheFlow(t *testing.T) {
	as := newMetadataServer(t, nil)
	c := NewClient(Config{ClientID: "cid", ClientSecret: "s", Issuer: as.srv.URL}, as.srv.URL+"#section", "", "")

	if _, err := c.ClientCredentials(context.Background()); err != nil {
		t.Fatalf("client credentials: %v", err)
	}
	form := <-as.tokenForms
	if got := form.Get("resource"); got != as.srv.URL {
		t.Fatalf("resource = %q, want the fragment-free resource %q", got, as.srv.URL)
	}
}

func TestDiscoveryFindsMetadataForAnIssuerWithAPath(t *testing.T) {
	// RFC 8414 inserts the well-known segment before the issuer path.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	issuer := srv.URL + "/tenant"

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": srv.URL, "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/tenant", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           issuer,
			"authorization_endpoint":           issuer + "/authorize",
			"token_endpoint":                   issuer + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})

	result, err := NewDiscovery().Discover(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if result.Server.Issuer != issuer || result.Server.TokenEndpoint != issuer+"/token" {
		t.Fatalf("metadata = %+v", result.Server)
	}
}

func TestTokenEndpointUnreachable(t *testing.T) {
	// The advertised token endpoint refuses connections: the failure surfaces as
	// an OAuth error rather than a panic, and no token is returned.
	as := newMetadataServer(t, map[string]any{"token_endpoint": "http://127.0.0.1:1/token"})
	c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL}, as.srv.URL, "", "")

	token, err := c.ClientCredentials(context.Background())
	if err == nil {
		t.Fatalf("expected a transport failure, got %+v", token)
	}
	if token != nil {
		t.Fatalf("token returned on failure: %+v", token)
	}
}

func TestRefreshPropagatesDiscoveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(Config{ClientID: "cid", Issuer: srv.URL}, srv.URL+"/mcp", "", "")
	if _, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "refresh", Issuer: srv.URL}); err == nil {
		t.Fatal("expected a discovery failure")
	}
	if _, err := c.ClientCredentials(context.Background()); err == nil {
		t.Fatal("expected a discovery failure")
	}
}

func TestTokenSetFromResponseFieldTypes(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		response  map[string]any
		wantTime  time.Time
		wantType  string
		wantScope string
	}{
		{
			name:     "expires_in seconds",
			response: map[string]any{"access_token": "a", "expires_in": float64(3600)},
			wantTime: now.Add(time.Hour),
			wantType: "Bearer",
		},
		{
			name:      "absent expiry",
			response:  map[string]any{"access_token": "a", "token_type": "DPoP", "scope": "mcp:use"},
			wantType:  "DPoP",
			wantScope: "mcp:use",
		},
		{
			name:     "non numeric expiry is ignored",
			response: map[string]any{"access_token": "a", "expires_in": "3600"},
			wantType: "Bearer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenSetFromResponse(tt.response, now)
			if !got.ExpiresAt.Equal(tt.wantTime) {
				t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, tt.wantTime)
			}
			if got.TokenType != tt.wantType || got.Scope != tt.wantScope {
				t.Fatalf("token = %+v", got)
			}
			if got.Expired(now.Add(-time.Second)) {
				t.Fatal("a token must not be expired before it was issued")
			}
			if tt.wantTime.IsZero() && got.Expired(now.Add(10*time.Hour)) {
				t.Fatal("a token without an expiry must never be reported as expired")
			}
		})
	}
}
