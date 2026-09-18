package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity/router"
)

// stubStore is a ClientStore that records what it was asked to create and
// answers with a fixed identifier, so the registration endpoint can be driven
// without any persistence.
type stubStore struct {
	mu        sync.Mutex
	got       []ClientRegistration
	id        string
	answer    RegisteredClient
	failWith  error
	useAnswer bool
}

func (s *stubStore) CreateClient(_ context.Context, reg ClientRegistration) (RegisteredClient, error) {
	s.mu.Lock()
	s.got = append(s.got, reg)
	s.mu.Unlock()

	if s.failWith != nil {
		return RegisteredClient{}, s.failWith
	}
	if s.useAnswer {
		return s.answer, nil
	}
	return RegisteredClient{ID: s.id}, nil
}

func (s *stubStore) registrations() []ClientRegistration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ClientRegistration(nil), s.got...)
}

// mount registers the discovery routes on a fresh router.
func mount(cfg Config) *router.VelocityRouterV2 {
	r := router.NewV2()
	Routes(r, cfg)
	return r
}

// get drives a GET through r and returns the recorder.
func get(r *router.VelocityRouterV2, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// decodeJSON decodes a recorder body into a generic document.
func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return doc
}

func TestRoutes_ProtectedResourceMetadata(t *testing.T) {
	const base = "https://mcp.example.test"

	tests := []struct {
		name   string
		cfg    Config
		target string
		want   map[string]any
	}{
		{
			name:   "root resource",
			cfg:    Config{BaseURL: base},
			target: "/.well-known/oauth-protected-resource",
			want: map[string]any{
				"resource":              base,
				"authorization_servers": []any{base},
				"scopes_supported":      []any{"mcp:use"},
			},
		},
		{
			name:   "single segment resource",
			cfg:    Config{BaseURL: base},
			target: "/.well-known/oauth-protected-resource/mcp",
			want: map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []any{base},
				"scopes_supported":      []any{"mcp:use"},
			},
		},
		{
			name:   "deeply nested resource",
			cfg:    Config{BaseURL: base},
			target: "/.well-known/oauth-protected-resource/tenants/acme/mcp/weather",
			want: map[string]any{
				"resource":              base + "/tenants/acme/mcp/weather",
				"authorization_servers": []any{base},
				"scopes_supported":      []any{"mcp:use"},
			},
		},
		{
			// RFC 9728 3.1 inserts the well-known path between the host and the
			// path of the resource identifier, so reading it back out is what
			// names the resource. /mcp/ and /mcp are two identifiers, and 3.3
			// has the client refuse a document that answers for the other one.
			name:   "a trailing slash is part of the resource identifier",
			cfg:    Config{BaseURL: base},
			target: "/.well-known/oauth-protected-resource/mcp/",
			want: map[string]any{
				"resource":              base + "/mcp/",
				"authorization_servers": []any{base},
				"scopes_supported":      []any{"mcp:use"},
			},
		},
		{
			// The identifier with an empty path and the one with a bare slash
			// are the same URI (RFC 3986 6.2.3), so both spellings are answered
			// with the one the client asked about.
			name:   "the root resource spelled with a slash",
			cfg:    Config{BaseURL: base},
			target: "/.well-known/oauth-protected-resource/",
			want: map[string]any{
				"resource":              base + "/",
				"authorization_servers": []any{base},
				"scopes_supported":      []any{"mcp:use"},
			},
		},
		{
			name:   "external authorization server and custom scope",
			cfg:    Config{BaseURL: base, AuthorizationServer: "https://id.example.test", Scope: "provisioning"},
			target: "/.well-known/oauth-protected-resource/mcp",
			want: map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []any{"https://id.example.test"},
				"scopes_supported":      []any{"provisioning"},
			},
		},
		{
			// RFC 8414 3.3 makes the client compare the issuer it discovers here
			// with the issuer in the authorization server's own metadata, byte
			// for byte. Issuers that end in a slash are real (a hosted identity
			// tenant is the common case), so the configured value is advertised
			// exactly as given rather than tidied up.
			name:   "an issuer ending in a slash keeps it",
			cfg:    Config{BaseURL: base, AuthorizationServer: "https://tenant.auth0.test/"},
			target: "/.well-known/oauth-protected-resource/mcp",
			want: map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []any{"https://tenant.auth0.test/"},
				"scopes_supported":      []any{"mcp:use"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := get(mount(tt.cfg), tt.target)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			got := decodeJSON(t, w)
			if len(got) != len(tt.want) {
				t.Fatalf("document = %v, want exactly the keys %v", got, tt.want)
			}
			for key, want := range tt.want {
				if !jsonEqual(got[key], want) {
					t.Errorf("%s = %#v, want %#v", key, got[key], want)
				}
			}
		})
	}
}

// The resource path arrives from the URL, so a caller can put anything in it.
// Whatever comes back has to stay a syntactically valid URL.
func TestRoutes_ProtectedResourceEscapesHostileResourcePath(t *testing.T) {
	r := mount(Config{BaseURL: "https://mcp.example.test"})

	w := get(r, "/.well-known/oauth-protected-resource/mcp/a%20b/%22quoted%22")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	resource, _ := decodeJSON(t, w)["resource"].(string)
	const want = "https://mcp.example.test/mcp/a%20b/%22quoted%22"
	if resource != want {
		t.Fatalf("resource = %q, want %q", resource, want)
	}
}

func TestRoutes_AuthorizationServerMetadata(t *testing.T) {
	const base = "https://mcp.example.test"
	store := &stubStore{id: "client-1"}

	tests := []struct {
		name   string
		cfg    Config
		target string
		want   map[string]any
	}{
		{
			name: "relative endpoints are resolved against the origin",
			cfg: Config{
				BaseURL:               base,
				AuthorizationEndpoint: "/oauth/authorize",
				TokenEndpoint:         "/oauth/token",
			},
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                           base,
				"authorization_endpoint":           base + "/oauth/authorize",
				"token_endpoint":                   base + "/oauth/token",
				"response_types_supported":         []any{"code"},
				"code_challenge_methods_supported": []any{"S256"},
				"scopes_supported":                 []any{"mcp:use"},
				"grant_types_supported":            []any{"authorization_code", "refresh_token"},
			},
		},
		{
			name: "absolute endpoints are passed through and registration is advertised",
			cfg: Config{
				BaseURL:               base,
				AuthorizationEndpoint: "https://id.example.test/authorize",
				TokenEndpoint:         "https://id.example.test/token",
				AuthorizationServer:   "https://id.example.test",
				Prefix:                "custom-oauth",
				Clients:               store,
			},
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                           "https://id.example.test",
				"authorization_endpoint":           "https://id.example.test/authorize",
				"token_endpoint":                   "https://id.example.test/token",
				"registration_endpoint":            base + "/custom-oauth/register",
				"response_types_supported":         []any{"code"},
				"code_challenge_methods_supported": []any{"S256"},
				"scopes_supported":                 []any{"mcp:use"},
				"grant_types_supported":            []any{"authorization_code", "refresh_token"},
				// Every client this endpoint registers is a public one, and an
				// absent list would read as client_secret_basic alone (RFC
				// 8414 2).
				"token_endpoint_auth_methods_supported": []any{"none"},
			},
		},
		{
			// RFC 9207 2.3: a server that puts iss on its authorization
			// responses has to say so here, or no client can tell a response
			// whose iss was stripped from one that never carried any.
			name: "an authorization endpoint that sends iss advertises it",
			cfg: Config{
				BaseURL:                     base,
				AuthorizationEndpoint:       "/oauth/authorize",
				TokenEndpoint:               "/oauth/token",
				AuthorizationResponseIssuer: true,
			},
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                                         base,
				"authorization_endpoint":                         base + "/oauth/authorize",
				"token_endpoint":                                 base + "/oauth/token",
				"response_types_supported":                       []any{"code"},
				"code_challenge_methods_supported":               []any{"S256"},
				"scopes_supported":                               []any{"mcp:use"},
				"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
				"authorization_response_iss_parameter_supported": true,
			},
		},
		{
			name: "an authorization endpoint that accepts client id metadata documents advertises it",
			cfg: Config{
				BaseURL:                   base,
				AuthorizationEndpoint:     "/oauth/authorize",
				TokenEndpoint:             "/oauth/token",
				ClientIDMetadataDocuments: true,
			},
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                                base,
				"authorization_endpoint":                base + "/oauth/authorize",
				"token_endpoint":                        base + "/oauth/token",
				"response_types_supported":              []any{"code"},
				"code_challenge_methods_supported":      []any{"S256"},
				"scopes_supported":                      []any{"mcp:use"},
				"grant_types_supported":                 []any{"authorization_code", "refresh_token"},
				"client_id_metadata_document_supported": true,
			},
		},
		{
			name: "configured token endpoint authentication methods replace the default",
			cfg: Config{
				BaseURL:                  base,
				AuthorizationEndpoint:    "/oauth/authorize",
				TokenEndpoint:            "/oauth/token",
				Clients:                  store,
				TokenEndpointAuthMethods: []string{"none", "client_secret_post", "", "none"},
			},
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                                base,
				"authorization_endpoint":                base + "/oauth/authorize",
				"token_endpoint":                        base + "/oauth/token",
				"registration_endpoint":                 base + "/oauth/register",
				"response_types_supported":              []any{"code"},
				"code_challenge_methods_supported":      []any{"S256"},
				"scopes_supported":                      []any{"mcp:use"},
				"grant_types_supported":                 []any{"authorization_code", "refresh_token"},
				"token_endpoint_auth_methods_supported": []any{"none", "client_secret_post"},
			},
		},
		{
			name: "everything at once, on the nested route too",
			cfg: Config{
				BaseURL:                     base,
				AuthorizationEndpoint:       "/oauth/authorize",
				TokenEndpoint:               "/oauth/token",
				AuthorizationResponseIssuer: true,
				ClientIDMetadataDocuments:   true,
				TokenEndpointAuthMethods:    []string{"client_secret_basic"},
			},
			target: "/.well-known/oauth-authorization-server/mcp",
			want: map[string]any{
				"issuer":                                         base,
				"authorization_endpoint":                         base + "/oauth/authorize",
				"token_endpoint":                                 base + "/oauth/token",
				"response_types_supported":                       []any{"code"},
				"code_challenge_methods_supported":               []any{"S256"},
				"scopes_supported":                               []any{"mcp:use"},
				"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
				"token_endpoint_auth_methods_supported":          []any{"client_secret_basic"},
				"authorization_response_iss_parameter_supported": true,
				"client_id_metadata_document_supported":          true,
			},
		},
		{
			// Same rule as the protected-resource document: the issuer this
			// server publishes is the one a client will compare against, so a
			// trailing slash is part of it.
			name: "the configured issuer is published verbatim",
			cfg: Config{
				BaseURL:               base,
				AuthorizationServer:   "https://tenant.auth0.test/",
				AuthorizationEndpoint: "https://tenant.auth0.test/authorize",
				TokenEndpoint:         "https://tenant.auth0.test/oauth/token",
			},
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                           "https://tenant.auth0.test/",
				"authorization_endpoint":           "https://tenant.auth0.test/authorize",
				"token_endpoint":                   "https://tenant.auth0.test/oauth/token",
				"response_types_supported":         []any{"code"},
				"code_challenge_methods_supported": []any{"S256"},
				"scopes_supported":                 []any{"mcp:use"},
				"grant_types_supported":            []any{"authorization_code", "refresh_token"},
			},
		},
		{
			name: "the nested route serves the same document",
			cfg: Config{
				BaseURL:               base,
				AuthorizationEndpoint: "/oauth/authorize",
				TokenEndpoint:         "/oauth/token",
			},
			target: "/.well-known/oauth-authorization-server/mcp/weather",
			want: map[string]any{
				"issuer":                           base,
				"authorization_endpoint":           base + "/oauth/authorize",
				"token_endpoint":                   base + "/oauth/token",
				"response_types_supported":         []any{"code"},
				"code_challenge_methods_supported": []any{"S256"},
				"scopes_supported":                 []any{"mcp:use"},
				"grant_types_supported":            []any{"authorization_code", "refresh_token"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := get(mount(tt.cfg), tt.target)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			got := decodeJSON(t, w)
			if len(got) != len(tt.want) {
				t.Fatalf("document = %v, want exactly the keys %v", got, tt.want)
			}
			for key, want := range tt.want {
				if !jsonEqual(got[key], want) {
					t.Errorf("%s = %#v, want %#v", key, got[key], want)
				}
			}
		})
	}
}

// Config.Scope is the space-delimited scope value of RFC 6749 3.3, while
// scopes_supported is a JSON array carrying one element per scope (RFC 8414 2,
// RFC 9728 2). A client checking whether the resource understands a single
// permission looks for it as its own element, so both documents have to split
// the configured value rather than advertise it whole.
func TestRoutes_MetadataAdvertisesOneElementPerScope(t *testing.T) {
	const base = "https://mcp.example.test"

	tests := []struct {
		name  string
		scope string
		want  []any
	}{
		{
			name:  "one scope",
			scope: "mcp:use",
			want:  []any{"mcp:use"},
		},
		{
			name:  "two scopes become two elements",
			scope: "read write",
			want:  []any{"read", "write"},
		},
		{
			name:  "padding and runs of spaces do not become elements",
			scope: "  read   write  ",
			want:  []any{"read", "write"},
		},
		{
			name:  "a repeated scope is advertised once",
			scope: "read write read",
			want:  []any{"read", "write"},
		},
		{
			name:  "a value no client could request is dropped",
			scope: "read ba\"d wri\\te write",
			want:  []any{"read", "write"},
		},
		{
			name:  "nothing requestable leaves the member out",
			scope: `" \`,
			want:  nil,
		},
	}

	documents := []struct {
		kind   string
		cfg    func(scope string) Config
		target string
	}{
		{
			kind: "protected resource",
			cfg: func(scope string) Config {
				return Config{BaseURL: base, Scope: scope}
			},
			target: "/.well-known/oauth-protected-resource/mcp",
		},
		{
			kind: "authorization server",
			cfg: func(scope string) Config {
				return Config{
					BaseURL:               base,
					Scope:                 scope,
					AuthorizationEndpoint: "/oauth/authorize",
					TokenEndpoint:         "/oauth/token",
				}
			},
			target: "/.well-known/oauth-authorization-server",
		},
	}

	for _, doc := range documents {
		for _, tt := range tests {
			t.Run(doc.kind+"/"+tt.name, func(t *testing.T) {
				w := get(mount(doc.cfg(tt.scope)), doc.target)

				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
				}
				got := decodeJSON(t, w)

				if tt.want == nil {
					if value, present := got["scopes_supported"]; present {
						t.Fatalf("scopes_supported = %#v, want the member omitted", value)
					}
					return
				}
				if !jsonEqual(got["scopes_supported"], tt.want) {
					t.Errorf("scopes_supported = %#v, want %#v", got["scopes_supported"], tt.want)
				}
			})
		}
	}
}

// A resource server that fronts somebody else's issuer has no endpoints of its
// own, so it must not publish an authorization-server document at all rather
// than publish one missing the fields RFC 8414 requires.
func TestRoutes_AuthorizationServerUnmountedWithoutEndpoints(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no endpoints at all", Config{BaseURL: "https://mcp.example.test"}},
		{"only an authorization endpoint", Config{BaseURL: "https://mcp.example.test", AuthorizationEndpoint: "/oauth/authorize"}},
		{"only a token endpoint", Config{BaseURL: "https://mcp.example.test", TokenEndpoint: "/oauth/token"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := mount(tt.cfg)

			for _, target := range []string{
				"/.well-known/oauth-authorization-server",
				"/.well-known/oauth-authorization-server/mcp",
			} {
				if w := get(r, target); w.Code != http.StatusNotFound {
					t.Errorf("GET %s status = %d, want 404", target, w.Code)
				}
			}
			// Protected-resource metadata is still published: that is what
			// points the client at the external issuer.
			if w := get(r, "/.well-known/oauth-protected-resource/mcp"); w.Code != http.StatusOK {
				t.Errorf("protected resource status = %d, want 200", w.Code)
			}
		})
	}
}

// An application that declares it publishes no protected-resource metadata must
// not have this package publish some anyway: the challenge advertises none, so a
// mounted document would contradict it.
func TestRoutes_ProtectedResourceUnmountedWhenDeclaredAbsent(t *testing.T) {
	r := mount(Config{
		BaseURL:                 "https://mcp.example.test",
		AuthorizationEndpoint:   "/oauth/authorize",
		TokenEndpoint:           "/oauth/token",
		WithoutResourceMetadata: true,
	})

	for _, target := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		if w := get(r, target); w.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", target, w.Code)
		}
	}
	// The authorization-server documents are a separate decision and stay.
	if w := get(r, "/.well-known/oauth-authorization-server"); w.Code != http.StatusOK {
		t.Errorf("authorization server status = %d, want 200", w.Code)
	}
}

func TestRoutes_RegistrationUnmountedWithoutStore(t *testing.T) {
	r := mount(Config{BaseURL: "https://mcp.example.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader("{}")))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no client store backs registration", w.Code)
	}
}

// An application that already serves its own discovery document keeps it: this
// package fills gaps, it does not take routes over.
func TestRoutes_LeavesAnApplicationsOwnDocumentAlone(t *testing.T) {
	r := router.NewV2()
	r.Get("/.well-known/oauth-protected-resource", func(c *router.Context) error {
		return c.JSON(http.StatusOK, map[string]any{"mine": true})
	})
	r.Get("/.well-known/oauth-authorization-server", func(c *router.Context) error {
		return c.JSON(http.StatusOK, map[string]any{"mine": true})
	})

	Routes(r, Config{
		BaseURL:               "https://mcp.example.test",
		AuthorizationEndpoint: "/oauth/authorize",
		TokenEndpoint:         "/oauth/token",
	})

	for _, target := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
	} {
		w := get(r, target)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", target, w.Code)
		}
		if mine, _ := decodeJSON(t, w)["mine"].(bool); !mine {
			t.Errorf("GET %s served %s, want the application's own document", target, w.Body.String())
		}
	}

	// The nested documents are still mounted, so a client that derives the
	// metadata URL from a nested resource path is still answered.
	w := get(r, "/.well-known/oauth-protected-resource/mcp")
	if w.Code != http.StatusOK {
		t.Fatalf("nested status = %d, want 200", w.Code)
	}
	if resource, _ := decodeJSON(t, w)["resource"].(string); resource != "https://mcp.example.test/mcp" {
		t.Fatalf("nested resource = %q, want the derived resource identifier", resource)
	}
}

// Mounting twice must not register duplicates: velocity rejects a repeated
// route name, so a second call has to be a no-op rather than a panic.
func TestRoutes_MountingTwiceIsANoOp(t *testing.T) {
	cfg := Config{
		BaseURL:               "https://mcp.example.test",
		AuthorizationEndpoint: "/oauth/authorize",
		TokenEndpoint:         "/oauth/token",
		Clients:               &stubStore{id: "client-1"},
	}

	r := router.NewV2()
	Routes(r, cfg)
	before := len(r.AllRoutes())
	Routes(r, cfg)

	if after := len(r.AllRoutes()); after != before {
		t.Fatalf("route count = %d after a second mount, want %d", after, before)
	}
	if w := get(r, "/.well-known/oauth-protected-resource/mcp"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the routes still to serve", w.Code)
	}
}

// A nil router has nothing to mount on, and library code does not panic over
// it, whatever the configuration would have mounted.
func TestRoutes_NilRouterIsIgnored(t *testing.T) {
	configs := map[string]Config{
		"the zero config": {},
		"every route configured": {
			BaseURL:               "https://mcp.example.test",
			AuthorizationEndpoint: "/oauth/authorize",
			TokenEndpoint:         "/oauth/token",
			Clients:               &stubStore{id: "client-1"},
		},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Routes panicked on a nil router: %v", r)
				}
			}()
			Routes(nil, cfg)

			if got := registeredRoutes(nil); len(got.paths) != 0 || len(got.names) != 0 {
				t.Fatalf("a nil router reported routes: %+v", got)
			}
		})
	}
}

// The discovery documents are read concurrently by every client that hits a
// 401, and one Config value serves them all.
func TestRoutes_ConcurrentDiscovery(t *testing.T) {
	r := mount(Config{
		BaseURL:               "https://mcp.example.test",
		AuthorizationEndpoint: "/oauth/authorize",
		TokenEndpoint:         "/oauth/token",
	})

	targets := []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server",
	}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		target := targets[i%len(targets)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := get(r, target); w.Code != http.StatusOK {
				t.Errorf("GET %s status = %d, want 200", target, w.Code)
			}
		}()
	}
	wg.Wait()
}

// jsonEqual compares a decoded JSON value against an expectation, element by
// element for arrays so a slice mismatch reports usefully.
func jsonEqual(got, want any) bool {
	wantList, ok := want.([]any)
	if !ok {
		return got == want
	}
	gotList, ok := got.([]any)
	if !ok || len(gotList) != len(wantList) {
		return false
	}
	for i := range wantList {
		if gotList[i] != wantList[i] {
			return false
		}
	}
	return true
}
