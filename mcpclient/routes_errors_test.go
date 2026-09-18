package mcpclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// noPKCEServer advertises an authorization server that does not support PKCE,
// which the client must refuse.
func noPKCEServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		})
	})
	return srv
}

func TestRedirectRefusesAServerWithoutPKCE(t *testing.T) {
	// 502 alone would not prove the refusal is the PKCE one: every upstream
	// failure of the authorization start answers 502. The unreachable server in
	// the second row is the control - it also fails, but for another reason, so
	// only the first row may name the missing metadata field.
	tests := []struct {
		name          string
		resourceURL   func(t *testing.T) string
		wantPKCECause bool
	}{
		{
			name:          "server without advertised pkce",
			resourceURL:   func(t *testing.T) string { return noPKCEServer(t).URL + "/mcp" },
			wantPKCECause: true,
		},
		{
			name: "unreachable server",
			resourceURL: func(t *testing.T) string {
				srv := httptest.NewServer(http.NotFoundHandler())
				url := srv.URL + "/mcp"
				srv.Close()
				return url
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := fmt.Sprintf("nopkce-%d", i)
			resourceURL := tt.resourceURL(t)
			RegisterClient(name, resourceURL)
			p := OAuthRoutesFor(name, oauth.Config{ClientID: "cid", Issuer: strings.TrimSuffix(resourceURL, "/mcp")}, WithStore(NewMemoryStore()))
			call := mountModule(t, p)

			rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/"+name+"/redirect")

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Location") != "" {
				t.Fatalf("a refused flow must not redirect the browser: %q", rec.Header().Get("Location"))
			}
			named := strings.Contains(rec.Body.String(), "code_challenge_methods_supported")
			if named != tt.wantPKCECause {
				t.Fatalf("body names code_challenge_methods_supported = %v, want %v; body: %s",
					named, tt.wantPKCECause, rec.Body.String())
			}
		})
	}
}

// TestCallbackDoesNotShowAnErrorFromAnUnvalidatedResponse drives the callback
// route with an error response, which the route answers by printing the failure
// to the browser. RFC 9207 2.4 and the MCP authorization specification forbid
// displaying the error or error_description of a response whose iss does not
// name the authorization server the flow was started with, so that text may
// reach the page only once the response has been shown to come from it.
func TestCallbackDoesNotShowAnErrorFromAnUnvalidatedResponse(t *testing.T) {
	const planted = "call +1 555 0100 to re-verify your account"

	tests := []struct {
		name      string
		iss       func(issuer string) string
		wantShown bool
	}{
		{name: "iss of another server", iss: func(string) string { return "https://evil.example" }},
		{name: "iss absent although the server advertises it", iss: func(string) string { return "" }},
		{name: "iss differing by a trailing slash", iss: func(issuer string) string { return issuer + "/" }},
		{name: "iss of the server the flow started with", iss: func(issuer string) string { return issuer }, wantShown: true},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := fakeAS(t) // advertises authorization_response_iss_parameter_supported
			name := fmt.Sprintf("unvalidated-%d", i)
			RegisterClient(name, as.URL+"/mcp")
			store := &failingStore{}
			call := mountModule(t, OAuthRoutesFor(name, oauth.Config{ClientID: "cid", Issuer: as.URL}, WithStore(store)))

			state := startFlow(t, call, "http://localhost:4000", "/mcp/oauth/"+name+"/redirect").Get("state")
			target := "http://localhost:4000/mcp/oauth/" + name + "/callback?error=access_denied" +
				"&error_description=" + url.QueryEscape(planted) + "&state=" + url.QueryEscape(state)
			if iss := tt.iss(as.URL); iss != "" {
				target += "&iss=" + url.QueryEscape(iss)
			}
			rec := call(http.MethodGet, target)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Location") != "" {
				t.Fatalf("a failed callback must not redirect the browser: %q", rec.Header().Get("Location"))
			}
			body := rec.Body.String()
			if shown := strings.Contains(body, planted); shown != tt.wantShown {
				t.Fatalf("error_description shown = %v, want %v; body: %s", shown, tt.wantShown, body)
			}
			if shown := strings.Contains(body, "access_denied"); shown != tt.wantShown {
				t.Fatalf("error code shown = %v, want %v; body: %s", shown, tt.wantShown, body)
			}
		})
	}
}

// A pre-registered client id exists at the one authorization server it was
// registered with, and the MCP authorization specification has it keyed by that
// issuer. The MCP server is what names the authorization server of a flow, so a
// configuration that does not say which one the client id belongs to is refused
// before the browser is sent anywhere, and one that does is sent there only.
func TestRedirectBindsAPreRegisteredClientToItsIssuer(t *testing.T) {
	tests := []struct {
		name       string
		issuer     func(as string) string
		wantStatus int
		wantBody   string
	}{
		{name: "no issuer configured", issuer: func(string) string { return "" }, wantStatus: http.StatusBadGateway, wantBody: "Config.Issuer must name the authorization server"},
		{name: "another issuer configured", issuer: func(string) string { return "https://auth.example.com" }, wantStatus: http.StatusBadGateway, wantBody: "belong to a different authorization server"},
		{name: "the advertised issuer configured", issuer: func(as string) string { return as }, wantStatus: http.StatusFound},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := fakeAS(t)
			name := fmt.Sprintf("bound-%d", i)
			RegisterClient(name, as.URL+"/mcp")
			store := &failingStore{}
			p := OAuthRoutesFor(name, oauth.Config{ClientID: "cid", Issuer: tt.issuer(as.URL)}, WithStore(store))
			call := mountModule(t, p)

			rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/"+name+"/redirect")

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusFound {
				if !strings.HasPrefix(rec.Header().Get("Location"), as.URL+"/authorize?") {
					t.Fatalf("Location = %q, want the configured authorization server", rec.Header().Get("Location"))
				}
				return
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want mention of %q", rec.Body.String(), tt.wantBody)
			}
			if rec.Header().Get("Location") != "" {
				t.Fatalf("a refused flow must not redirect the browser: %q", rec.Header().Get("Location"))
			}
			if store.pending != nil {
				t.Fatalf("a refused flow persisted a pending authorization: %+v", store.pending)
			}
		})
	}
}

func TestRoutesAreNamed(t *testing.T) {
	p := OAuthRoutesFor("github", oauth.Config{}, WithPublicURL("https://app.example.com"))
	r := router.NewV2()
	stack := chain.NewMiddlewareStack(&velapp.Services{})
	p.Routes(chain.NewRouting(r, stack))

	// Names are only resolvable once the router has committed its routes.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost:4000/", nil))

	for name, want := range map[string]string{
		"mcp.oauth.github.redirect":        "/mcp/oauth/github/redirect",
		"mcp.oauth.github.callback":        "/mcp/oauth/github/callback",
		"mcp.oauth.github.client-metadata": "/mcp/oauth/github/client-metadata.json",
	} {
		got, err := r.RouteURL(name, nil)
		if err != nil {
			t.Fatalf("RouteURL(%q): %v", name, err)
		}
		if got != want {
			t.Fatalf("RouteURL(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestHandlersRejectAnUnregisteredClient(t *testing.T) {
	p := OAuthRoutesFor("never-registered", oauth.Config{ClientID: "cid"}, WithStore(NewMemoryStore()))
	call := mountModule(t, p)

	for _, path := range []string{
		"http://localhost:4000/mcp/oauth/never-registered/redirect",
		"http://localhost:4000/mcp/oauth/never-registered/callback?state=x",
	} {
		rec := call(http.MethodGet, path)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, want 500", path, rec.Code)
		}
	}

	// The metadata document does not depend on the registry, so it is still
	// published: an authorization server may fetch it at any time.
	if rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/never-registered/client-metadata.json"); rec.Code != http.StatusOK {
		t.Fatalf("document status = %d, want 200", rec.Code)
	}
}

func TestClientMetadataRedirectURIShapes(t *testing.T) {
	// redirect_uris comes from the consumer, either as Go values or as whatever
	// a decoded JSON document produced, so every shape has to land on a list of
	// usable URIs. The callback this module serves always leads, because it is
	// the one the authorization server will be told to redirect to.
	const callback = "https://app.example.com/mcp/oauth/shapes/callback"

	tests := []struct {
		name  string
		value any
		want  []string
	}{
		{
			name:  "decoded json array",
			value: []any{"https://app.example.com/extra", 42, "", "https://app.example.com/extra"},
			want:  []string{callback, "https://app.example.com/extra"},
		},
		{
			name:  "decoded json array of holes only",
			value: []any{"", nil, 42, true},
			want:  []string{callback},
		},
		{
			name:  "string slice",
			value: []string{"https://app.example.com/extra"},
			want:  []string{callback, "https://app.example.com/extra"},
		},
		{
			name:  "string slice with empty entries",
			value: []string{"", "https://app.example.com/extra", ""},
			want:  []string{callback, "https://app.example.com/extra"},
		},
		{
			name:  "string slice of empty entries only",
			value: []string{"", ""},
			want:  []string{callback},
		},
		{
			name:  "string slice repeating the callback",
			value: []string{callback, "https://app.example.com/extra", "https://app.example.com/extra"},
			want:  []string{callback, "https://app.example.com/extra"},
		},
		{
			name:  "empty string slice",
			value: []string{},
			want:  []string{callback},
		},
		{
			name:  "single string",
			value: "https://app.example.com/only",
			want:  []string{callback, "https://app.example.com/only"},
		},
		{
			name:  "wrong type is ignored",
			value: 42,
			want:  []string{callback},
		},
		{
			name:  "empty string is ignored",
			value: "",
			want:  []string{callback},
		},
		{
			name:  "nothing declared",
			value: nil,
			want:  []string{callback},
		},
		{
			name:  "unicode path is published as given",
			value: []string{"https://app.example.com/rücksprung"},
			want:  []string{callback, "https://app.example.com/rücksprung"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := OAuthRoutesFor("shapes", oauth.Config{},
				WithPublicURL("https://app.example.com"),
				WithClientMetadata(map[string]any{"redirect_uris": tt.value}))
			call := mountModule(t, p)

			doc := decodeDocument(t, call(http.MethodGet, "https://app.example.com/mcp/oauth/shapes/client-metadata.json"))
			wantStrings(t, doc["redirect_uris"], tt.want)
		})
	}
}

func TestClientMetadataPathOptionsAreNormalized(t *testing.T) {
	tests := []struct {
		name    string
		options []Option
		path    string
	}{
		{
			name:    "empty override keeps the default path",
			options: []Option{WithClientMetadataPath("")},
			path:    "/mcp/oauth/paths/client-metadata.json",
		},
		{
			name:    "override without a leading slash",
			options: []Option{WithClientMetadataPath("oauth/paths/metadata.json")},
			path:    "/oauth/paths/metadata.json",
		},
		{
			name:    "override with a trailing slash",
			options: []Option{WithClientMetadataPath("/oauth/paths/metadata/")},
			path:    "/oauth/paths/metadata",
		},
		{
			name:    "base path override moves the default",
			options: []Option{WithBasePath("/auth")},
			path:    "/auth/paths/client-metadata.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := OAuthRoutesFor("paths", oauth.Config{}, append(tt.options, WithPublicURL("https://app.example.com"))...)
			call := mountModule(t, p)

			rec := call(http.MethodGet, "https://app.example.com"+tt.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d for %s, want 200", rec.Code, tt.path)
			}
			doc := decodeDocument(t, rec)
			if got := doc["client_id"]; got != "https://app.example.com"+tt.path {
				t.Fatalf("client_id = %#v, want %q", got, "https://app.example.com"+tt.path)
			}
		})
	}
}
