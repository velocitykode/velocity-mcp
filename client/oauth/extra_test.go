package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestErrorTypes(t *testing.T) {
	cause := errors.New("boom")
	err := wrapError(cause, "wrapped %s", "msg")
	if err.Error() != "wrapped msg: boom" || !errors.Is(err, cause) {
		t.Fatalf("wrapError = %q", err.Error())
	}
	if newError("plain").Error() != "plain" {
		t.Fatal("plain message")
	}

	var nilErr *Error
	if nilErr.Error() != "<nil oauth error>" {
		t.Fatal("nil-safe Error")
	}

	authErr := &AuthorizationRequiredError{
		Message:   "need auth",
		Challenge: &Challenge{ResourceMetadataURL: "https://a/x", Scope: "s"},
	}
	if authErr.Error() != "need auth" || authErr.ResourceMetadataURL() != "https://a/x" || authErr.Scope() != "s" {
		t.Fatalf("auth err = %+v", authErr)
	}
	var nilAuth *AuthorizationRequiredError
	if nilAuth.ResourceMetadataURL() != "" || nilAuth.Scope() != "" {
		t.Fatal("nil-safe AuthorizationRequiredError accessors")
	}
}

func TestOrigin(t *testing.T) {
	got, err := origin("https://host.example.com:8443/path?x=1")
	if err != nil || got != "https://host.example.com:8443" {
		t.Fatalf("origin = %q err=%v", got, err)
	}
	if _, err := origin("://broken"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestValidateIssuerMissingWhenSupported(t *testing.T) {
	// Server advertises iss support but the callback omits it: reject.
	err := validateIssuer(&PendingAuthorization{Issuer: "https://as", IssuerSupported: true}, "")
	if err == nil {
		t.Fatal("expected missing-iss error")
	}
	// Not supported and omitted: allowed.
	if err := validateIssuer(&PendingAuthorization{Issuer: "https://as"}, ""); err != nil {
		t.Fatalf("missing iss without support should pass: %v", err)
	}
}

func TestDiscoverFallsBackToOriginIssuer(t *testing.T) {
	// Protected-resource metadata is absent (404, non-explicit), so discovery
	// falls back to the resource origin as issuer.
	var base string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
		})
	})

	result, err := NewDiscovery().Discover(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if result.Server.TokenEndpoint != base+"/token" {
		t.Fatalf("token endpoint = %q", result.Server.TokenEndpoint)
	}
}

func TestDiscoverExplicitMetadataFailureIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := NewDiscovery().Discover(context.Background(), srv.URL, srv.URL+"/explicit-metadata")
	if err == nil {
		t.Fatal("expected fatal error for explicit metadata failure")
	}
}

// metadataMux serves discovery metadata pointing token/register endpoints at the
// same server, letting a test override those handlers to exercise error paths.
func metadataMux(t *testing.T, codeChallengeMethods []string) (*http.ServeMux, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": srv.URL, "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/authorize",
			"token_endpoint":                   srv.URL + "/token",
			"registration_endpoint":            srv.URL + "/register",
			"code_challenge_methods_supported": codeChallengeMethods,
		})
	})
	return mux, srv
}

func TestTokenRequestFailure(t *testing.T) {
	mux, srv := metadataMux(t, []string{"S256"})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	c := NewClient(Config{ClientID: "cid"}, srv.URL, "", "")
	if _, err := c.ClientCredentials(context.Background()); err == nil {
		t.Fatal("expected token request failure")
	}
}

func TestTokenResponseMissingAccessToken(t *testing.T) {
	mux, srv := metadataMux(t, []string{"S256"})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"token_type": "Bearer"})
	})
	c := NewClient(Config{ClientID: "cid"}, srv.URL, "", "")
	if _, err := c.ClientCredentials(context.Background()); err == nil {
		t.Fatal("expected missing access_token error")
	}
}

func TestDynamicRegistrationFailure(t *testing.T) {
	mux, srv := metadataMux(t, []string{"S256"})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := NewClient(Config{RedirectURI: "http://localhost/cb"}, srv.URL, "", "")
	if _, _, err := c.AuthorizationURL(context.Background(), ""); err == nil {
		t.Fatal("expected registration failure")
	}
}

func TestAuthorizationURLRejectsMissingS256(t *testing.T) {
	// Server advertises only "plain" PKCE: the client requires S256.
	_, srv := metadataMux(t, []string{"plain"})
	c := NewClient(Config{ClientID: "cid", RedirectURI: "http://localhost/cb"}, srv.URL, "", "")
	if _, _, err := c.AuthorizationURL(context.Background(), ""); err == nil {
		t.Fatal("expected S256 requirement error")
	}
}

func TestDiscoverIssuerMismatch(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": srv.URL, "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 "https://someone-else.example.com",
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})
	if _, err := NewDiscovery().Discover(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected issuer mismatch error")
	}
}

func TestDiscoverMetadataMissingEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": srv.URL, "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"issuer": srv.URL}) // no endpoints
	})
	if _, err := NewDiscovery().Discover(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("expected metadata-missing-endpoints error")
	}
}

func TestDynamicRegistrationMissingClientID(t *testing.T) {
	mux, srv := metadataMux(t, []string{"S256"})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"client_secret": "s"}) // no client_id
	})
	c := NewClient(Config{RedirectURI: "http://localhost/cb"}, srv.URL, "", "")
	if _, _, err := c.AuthorizationURL(context.Background(), ""); err == nil {
		t.Fatal("expected missing client_id error")
	}
}
