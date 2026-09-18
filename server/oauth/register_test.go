package oauth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// registrationRequest builds the request a conforming client sends: a POST
// whose body is declared as application/json (RFC 7591 3.1).
func registrationRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// register drives one registration request through the real router and returns
// the recorder.
func register(t *testing.T, cfg Config, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := mount(cfg)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, registrationRequest(cfg.registerPath(), body))
	return w
}

// openConfig allows any http(s) redirect domain, the permissive posture an
// application opts into explicitly.
func openConfig(store ClientStore) Config {
	return Config{BaseURL: "https://mcp.example.test", Clients: store, RedirectDomains: []string{"*"}}
}

// A registration request is application/json (RFC 7591 3.1). On an endpoint
// nobody authenticates to, that is also what keeps a web page from driving it:
// text/plain, form and multipart bodies are the ones a browser sends to another
// origin without asking, so a page could otherwise register clients from every
// visitor's address. Anything that is not JSON is turned away before the body
// is read and before the store hears of it.
func TestRegister_RequiresAJSONContentType(t *testing.T) {
	const body = `{"redirect_uris":["https://app.example.test/callback"]}`

	tests := []struct {
		name        string
		contentType string
		chunked     bool
		wantStatus  int
	}{
		{name: "application/json", contentType: "application/json", wantStatus: http.StatusCreated},
		{name: "application/json with a charset", contentType: "application/json; charset=utf-8", wantStatus: http.StatusCreated},
		{name: "application/json in another case", contentType: "Application/JSON", wantStatus: http.StatusCreated},
		{name: "text/plain, which a form may send across origins", contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "a url-encoded form", contentType: "application/x-www-form-urlencoded", wantStatus: http.StatusUnsupportedMediaType},
		{name: "a multipart form", contentType: "multipart/form-data; boundary=x", wantStatus: http.StatusUnsupportedMediaType},
		{name: "no content type", contentType: "", wantStatus: http.StatusUnsupportedMediaType},
		{name: "no content type on a body of unknown length", contentType: "", chunked: true, wantStatus: http.StatusUnsupportedMediaType},
		{name: "a json-flavoured type", contentType: "application/problem+json", wantStatus: http.StatusUnsupportedMediaType},
		{name: "text/json", contentType: "text/json", wantStatus: http.StatusUnsupportedMediaType},
		{name: "a type that starts with application/json", contentType: "application/jsonp", wantStatus: http.StatusUnsupportedMediaType},
		{name: "two types in one header", contentType: "text/plain, application/json", wantStatus: http.StatusUnsupportedMediaType},
		{name: "json named only in a parameter", contentType: "text/plain; type=application/json", wantStatus: http.StatusUnsupportedMediaType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			cfg := openConfig(store)
			r := mount(cfg)

			req := httptest.NewRequest(http.MethodPost, cfg.registerPath(), strings.NewReader(body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.chunked {
				req.ContentLength = -1
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			wantSaved := 0
			if tt.wantStatus == http.StatusCreated {
				wantSaved = 1
			}
			if n := len(store.registrations()); n != wantSaved {
				t.Fatalf("store saw %d registrations, want %d", n, wantSaved)
			}
		})
	}
}

func TestRegister_IssuesAClient(t *testing.T) {
	store := &stubStore{id: "client-abc"}

	w := register(t, openConfig(store), `{
		"client_name": "Test Client",
		"redirect_uris": ["https://app.example.test/callback"],
		"logo_uri": "https://app.example.test/logo.png",
		"client_uri": "https://app.example.test"
	}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}

	var got registrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	want := registrationResponse{
		ClientID:                "client-abc",
		ClientName:              "Test Client",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		RedirectURIs:            []string{"https://app.example.test/callback"},
		Scope:                   "mcp:use",
		TokenEndpointAuthMethod: "none",
	}
	if !sameResponse(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}

	// The store never recorded the metadata, so the response must not claim it
	// did.
	if strings.Contains(w.Body.String(), "logo_uri") || strings.Contains(w.Body.String(), "client_uri") {
		t.Fatalf("body = %s, want no metadata echoed when the store kept none", w.Body.String())
	}

	regs := store.registrations()
	if len(regs) != 1 {
		t.Fatalf("store saw %d registrations, want 1", len(regs))
	}
	reg := regs[0]
	if reg.Name != "Test Client" {
		t.Errorf("registered name = %q, want %q", reg.Name, "Test Client")
	}
	if reg.LogoURI != "https://app.example.test/logo.png" || reg.ClientURI != "https://app.example.test" {
		t.Errorf("registered metadata = %q / %q, want both forwarded", reg.LogoURI, reg.ClientURI)
	}
	if reg.Scope != "mcp:use" {
		t.Errorf("registered scope = %q, want mcp:use", reg.Scope)
	}
}

// What the store says it recorded is what the client is told, so a store that
// normalizes, filters, or drops fields is reported honestly.
func TestRegister_EchoesWhatTheStoreRecorded(t *testing.T) {
	store := &stubStore{
		useAnswer: true,
		answer: RegisteredClient{
			ID:           "client-xyz",
			Name:         "app.example.test (normalized)",
			RedirectURIs: []string{"https://app.example.test/callback/normalized"},
			GrantTypes:   []string{"authorization_code"},
			LogoURI:      "https://app.example.test/logo.png",
			ClientURI:    "https://app.example.test",
		},
	}

	w := register(t, openConfig(store), `{"redirect_uris":["https://app.example.test/callback"],"logo_uri":"https://app.example.test/logo.png","client_uri":"https://app.example.test"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}

	var got registrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := registrationResponse{
		ClientID:                "client-xyz",
		ClientName:              "app.example.test (normalized)",
		GrantTypes:              []string{"authorization_code"},
		ResponseTypes:           []string{"code"},
		RedirectURIs:            []string{"https://app.example.test/callback/normalized"},
		Scope:                   "mcp:use",
		TokenEndpointAuthMethod: "none",
		LogoURI:                 "https://app.example.test/logo.png",
		ClientURI:               "https://app.example.test",
	}
	if !sameResponse(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
}

// The name a client is registered under is metadata this server provisions
// when the client sends none, so RFC 7591 3.2.1 has it reported back in the
// client information response as well as handed to the store.
func TestRegister_ResolvesTheClientName(t *testing.T) {
	tests := []struct {
		name string
		body string
		// storeName is what the store reports having recorded, for a store
		// that normalizes the name it was handed.
		storeName string
		// want is the name the store is handed; wantReported is the name on
		// the wire, which is the store's own when it reported one.
		want         string
		wantReported string
	}{
		{name: "client_name wins", body: `{"client_name":"Primary","name":"Alias","redirect_uris":["https://app.example.test/cb"]}`, want: "Primary", wantReported: "Primary"},
		{name: "name is the fallback alias", body: `{"name":"Alias","redirect_uris":["https://app.example.test/cb"]}`, want: "Alias", wantReported: "Alias"},
		{name: "redirect host when unnamed", body: `{"redirect_uris":["https://app.example.test/cb"]}`, want: "app.example.test", wantReported: "app.example.test"},
		{name: "custom scheme host when unnamed", body: `{"redirect_uris":["myapp://callback/done"]}`, want: "callback", wantReported: "callback"},
		{name: "blank names fall through to the host", body: `{"client_name":"","name":"","redirect_uris":["https://app.example.test/cb"]}`, want: "app.example.test", wantReported: "app.example.test"},
		// The RFC 8252 7.1 form has no authority at all, so there is no host to
		// borrow a name from.
		{name: "a redirect with no host to borrow falls back", body: `{"redirect_uris":["myapp:/oauth2redirect/provider"]}`, want: fallbackClientName, wantReported: fallbackClientName},
		{
			name:         "the store's own name is what is reported",
			body:         `{"client_name":"Primary","redirect_uris":["https://app.example.test/cb"]}`,
			storeName:    "Primary (2)",
			want:         "Primary",
			wantReported: "Primary (2)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			if tt.storeName != "" {
				store.useAnswer = true
				store.answer = RegisteredClient{ID: "client-1", Name: tt.storeName}
			}
			cfg := openConfig(store)
			cfg.CustomSchemes = []string{"myapp"}

			w := register(t, cfg, tt.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
			}
			regs := store.registrations()
			if len(regs) != 1 {
				t.Fatalf("store saw %d registrations, want 1", len(regs))
			}
			if regs[0].Name != tt.want {
				t.Fatalf("name = %q, want %q", regs[0].Name, tt.want)
			}

			var got registrationResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if got.ClientName != tt.wantReported {
				t.Fatalf("client_name = %q, want %q", got.ClientName, tt.wantReported)
			}
		})
	}
}

func TestRegister_RejectsBadInput(t *testing.T) {
	longName := strings.Repeat("a", maxClientNameLength+1)
	longURL := "https://app.example.test/" + strings.Repeat("a", maxClientURLLength)

	tests := []struct {
		name     string
		cfg      func(ClientStore) Config
		body     string
		wantCode string
		wantDesc string
	}{
		{
			name:     "missing redirect_uris",
			cfg:      openConfig,
			body:     `{"client_name":"X"}`,
			wantCode: errInvalidRedirectURI,
		},
		{
			name:     "empty redirect_uris",
			cfg:      openConfig,
			body:     `{"redirect_uris":[]}`,
			wantCode: errInvalidRedirectURI,
		},
		{
			name:     "redirect_uris is not an array",
			cfg:      openConfig,
			body:     `{"redirect_uris":"https://app.example.test/cb"}`,
			wantCode: errInvalidRedirectURI,
		},
		{
			name:     "non-string redirect uri",
			cfg:      openConfig,
			body:     `{"redirect_uris":[42]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "empty redirect uri",
			cfg:      openConfig,
			body:     `{"redirect_uris":[""]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "scheme-less redirect uri",
			cfg:      openConfig,
			body:     `{"redirect_uris":["not-a-url"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "http redirect uri without a host",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https:///callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "the bad element is named by index",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb","not-a-url"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.1 is not a valid URL.",
		},
		{
			name:     "unlisted private-use scheme",
			cfg:      openConfig,
			body:     `{"redirect_uris":["cursor://callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "domain outside the allowlist",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			body:     `{"redirect_uris":["https://evil.example.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			name: "a lookalike prefix is not the allowed domain",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			body:     `{"redirect_uris":["https://app.example.test.evil.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			name: "an empty allowlist permits nothing",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s}
			},
			body:     `{"redirect_uris":["https://app.example.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			name: "the loopback allowance does not extend to https",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://localhost"}}
			},
			body:     `{"redirect_uris":["https://localhost:8080/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			name: "an empty allowlist entry matches nothing",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"", "https://app.example.test"}}
			},
			body:     `{"redirect_uris":["https://other.example.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			// The loopback allowance is read off the host alone, so a name that
			// merely starts with one is an ordinary internet host: plain HTTP
			// to it is refused before the domain policy is even consulted.
			name: "a loopback lookalike host is not loopback",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://localhost"}}
			},
			body:     `{"redirect_uris":["http://localhost.evil.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 must use https, or http only on a loopback address.",
		},
		{
			// The authorization response comes back over the redirect URI, so
			// a plaintext hop to a public host exposes the authorization code.
			// Authorizing the domain does not authorize the transport.
			name:     "a plain http callback under a wildcard allowlist",
			cfg:      openConfig,
			body:     `{"redirect_uris":["http://public.example/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 must use https, or http only on a loopback address.",
		},
		{
			name: "a plain http callback on an explicitly allowed origin",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://app.example.test"}}
			},
			body:     `{"redirect_uris":["http://app.example.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 must use https, or http only on a loopback address.",
		},
		{
			// An authority of nothing but a port names no server at all, on
			// either http scheme, so neither the transport rule nor the domain
			// allowlist ever gets a say.
			name:     "a plain http callback with no host at all",
			cfg:      openConfig,
			body:     `{"redirect_uris":["http://:8080/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "an https callback with no host at all",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://:8443/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// A private-use scheme must still name something to deliver to.
			name: "a listed private-use scheme with nothing after it",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor:"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// The loopback allowance is what lets a native client register a
			// port it only learns at runtime; it is not on unless the
			// application listed a loopback host itself.
			name: "loopback is refused when no loopback host is allowed",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			body:     `{"redirect_uris":["http://localhost:18293/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			name: "loopback by address is refused when no loopback host is allowed",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			body:     `{"redirect_uris":["http://127.0.0.1:29100/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			name: "ipv6 loopback is refused when no loopback host is allowed",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			body:     `{"redirect_uris":["http://[::1]:39201/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a permitted redirect domain.",
		},
		{
			// An authority marker with neither a host nor a path after it
			// leaves nothing for the operating system to hand the client.
			name: "a listed private-use scheme with an empty authority and no path",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor://"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "a listed private-use scheme whose authority is only a port",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor://:8080"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "a listed private-use scheme whose authority is only a port, with a path",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor://:8080/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// Userinfo names a credential, not a destination, and an authority
			// of nothing but userinfo is how a host is made to look present.
			name: "a listed private-use scheme carrying userinfo",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor://user@/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// Everything before the "@" is userinfo, so the host the response
			// would actually travel to is the one after it. A wildcard allows
			// any origin but still not a callback that names a credential.
			name:     "an https callback carrying userinfo",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://user@app.example.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// The delivery staying on loopback does not make the userinfo form
			// any more of a destination; it is refused on every scheme.
			name: "a loopback callback carrying userinfo",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://localhost"}}
			},
			body:     `{"redirect_uris":["http://app.example.test@localhost:8123/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// The allowed origin spelled as userinfo in front of another host:
			// refused for naming a credential, before the allowlist is asked
			// whether the string looks like the permitted domain.
			name: "an allowed origin worn as userinfo by another host",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			body:     `{"redirect_uris":["https://app.example.test@evil.test/cb"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "a listed private-use scheme given as an opaque uri",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor:callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "a private-use scheme other than the one configured",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"vscode"}}
			},
			body:     `{"redirect_uris":["cursor://anysphere.cursor-mcp/oauth/callback"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// RFC 6749 3.1.2: a redirection endpoint URI must not include a
			// fragment. The authorization server puts its own response there.
			name:     "a redirect uri carrying a fragment",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb#frag"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "a redirect uri ending in a bare fragment marker",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb#"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "a private-use redirect uri carrying a fragment",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			body:     `{"redirect_uris":["cursor://callback/done#frag"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "logo_uri is not a url",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"logo_uri":"not-a-url"}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "logo_uri uses an unsupported scheme",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"logo_uri":"ftp://app.example.test/logo.png"}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "logo_uri is not a string",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"logo_uri":["https://app.example.test/logo.png"]}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "logo_uri is too long",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"logo_uri":"` + longURL + `"}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "client_uri is not a url",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"client_uri":"not-a-url"}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "client_name is too long",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"client_name":"` + longName + `"}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "client_name is not a string",
			cfg:      openConfig,
			body:     `{"redirect_uris":["https://app.example.test/cb"],"client_name":7}`,
			wantCode: errInvalidClientMetadata,
		},
		{
			name:     "a redirect failure is reported ahead of a metadata failure",
			cfg:      openConfig,
			body:     `{"redirect_uris":["not-a-url"],"logo_uri":"not-a-url"}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			// The redirect is reported even when a field the validator visits
			// first also failed: fixing the callback is what unblocks the
			// client.
			name:     "a redirect failure outranks an earlier field's failure",
			cfg:      openConfig,
			body:     `{"client_name":123,"redirect_uris":["not-a-url"]}`,
			wantCode: errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name:     "a body that is not an object",
			cfg:      openConfig,
			body:     `["https://app.example.test/cb"]`,
			wantCode: errInvalidClientMetadata,
			wantDesc: "The registration request body could not be parsed.",
		},
		{
			name:     "malformed json",
			cfg:      openConfig,
			body:     `{"redirect_uris":[`,
			wantCode: errInvalidClientMetadata,
			wantDesc: "The registration request body could not be parsed.",
		},
		{
			name:     "an empty body",
			cfg:      openConfig,
			body:     ``,
			wantCode: errInvalidClientMetadata,
			wantDesc: "The registration request body could not be parsed.",
		},
		{
			name:     "a json null body",
			cfg:      openConfig,
			body:     `null`,
			wantCode: errInvalidClientMetadata,
			wantDesc: "The registration request body could not be parsed.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			w := register(t, tt.cfg(store), tt.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			var got registrationError
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if got.Error != tt.wantCode {
				t.Errorf("error = %q, want %q (description %q)", got.Error, tt.wantCode, got.ErrorDescription)
			}
			if tt.wantDesc != "" && got.ErrorDescription != tt.wantDesc {
				t.Errorf("error_description = %q, want %q", got.ErrorDescription, tt.wantDesc)
			}
			if got.ErrorDescription == "" {
				t.Error("error_description is empty, want a message the client can act on")
			}
			if n := len(store.registrations()); n != 0 {
				t.Errorf("store saw %d registrations, want none before validation passes", n)
			}
		})
	}
}

func TestRegister_AcceptsPermittedRedirects(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(ClientStore) Config
		uri  string
	}{
		{
			name: "an allowlisted origin",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			uri: "https://app.example.test/callback",
		},
		{
			name: "an allowlisted origin already ending in a slash",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test/"}}
			},
			uri: "https://app.example.test/callback",
		},
		{
			name: "loopback on any port once loopback is allowed",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://localhost"}}
			},
			uri: "http://127.0.0.1:53821/callback",
		},
		{
			name: "the ipv6 loopback",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://127.0.0.1/"}}
			},
			uri: "http://[::1]:53821/callback",
		},
		{
			name: "a listed private-use scheme",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"cursor"}}
			},
			uri: "cursor://anysphere.cursor-retrieval/oauth/callback",
		},
		{
			// The form RFC 8252 7.1 writes: a private-use scheme, no authority
			// component, the callback in the path. A native client spelling its
			// redirect the way the specification illustrates has to register.
			name: "the private-use form with no authority component",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"com.example.app"}}
			},
			uri: "com.example.app:/oauth2redirect/provider",
		},
		{
			name: "the private-use form with no authority and a bare path",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"com.example.app"}}
			},
			uri: "com.example.app:/",
		},
		{
			// The same callback written with an empty authority. A private-use
			// URI is routed on its scheme, so this reaches the same client with
			// the same path and refusing it would only refuse a spelling.
			name: "the private-use form with an empty authority",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"com.example.app"}}
			},
			uri: "com.example.app:///oauth2redirect/provider",
		},
		{
			// Loopback is the one place a plain-HTTP callback is safe, and it
			// is what a native client on a runtime-chosen port needs.
			name: "plain http on loopback once loopback is allowed",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"http://localhost"}}
			},
			uri: "http://localhost:8123/callback",
		},
		{
			name: "a private-use scheme is not subject to the domain allowlist",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"vscode"}, RedirectDomains: []string{"https://app.example.test"}}
			},
			uri: "vscode://anything/callback",
		},
		{
			// URI schemes are case-insensitive (RFC 3986 3.1), so neither side
			// of the comparison decides the casing.
			name: "a private-use scheme spelled with different case on each side",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, CustomSchemes: []string{"MyApp"}}
			},
			uri: "myapp://callback/done",
		},
		{
			name: "a query string is not a fragment",
			cfg: func(s ClientStore) Config {
				return Config{Clients: s, RedirectDomains: []string{"https://app.example.test"}}
			},
			uri: "https://app.example.test/callback?state=keep",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			w := register(t, tt.cfg(store), `{"redirect_uris":["`+tt.uri+`"]}`)

			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
			}
			regs := store.registrations()
			if len(regs) != 1 || len(regs[0].RedirectURIs) != 1 || regs[0].RedirectURIs[0] != tt.uri {
				t.Fatalf("store received %#v, want the single uri %q", regs, tt.uri)
			}
		})
	}
}

// An allowlist entry is honoured as it is written. One scoped to a path admits
// what lives under that path, so a redirect URI must not be able to start with
// the entry and be delivered somewhere else, which is what dot segments do once
// a browser resolves them (RFC 3986 5.2.4). And an entry that names a scheme
// names that scheme: https://localhost is an origin, not leave to register the
// unencrypted loopback callback the application never listed.
func TestRegister_HonoursARedirectDomainAsWritten(t *testing.T) {
	const (
		dotSegments  = "redirect_uris.0 must not contain \".\" or \"..\" path segments."
		notPermitted = "redirect_uris.0 is not a permitted redirect domain."
	)
	scoped := []string{"https://app.example.test/clients"}

	tests := []struct {
		name    string
		domains []string
		uri     string
		// wantDesc is the refusal; "" means the uri is accepted.
		wantDesc string
	}{
		{name: "under the path the entry names", domains: scoped, uri: "https://app.example.test/clients/acme/cb"},
		{name: "a dot inside a segment", domains: scoped, uri: "https://app.example.test/clients/v1.2/cb"},
		{name: "a segment that starts with dots", domains: scoped, uri: "https://app.example.test/clients/..well-known/cb"},
		{name: "a segment that ends with dots", domains: scoped, uri: "https://app.example.test/clients/cb.."},
		{name: "a segment of three dots", domains: scoped, uri: "https://app.example.test/clients/.../cb"},
		{name: "dots in the query", domains: scoped, uri: "https://app.example.test/clients/cb?next=../../x"},

		{name: "dot segments that climb out of the path", domains: scoped, uri: "https://app.example.test/clients/../../evil", wantDesc: dotSegments},
		{name: "one dot segment is enough", domains: scoped, uri: "https://app.example.test/clients/../evil", wantDesc: dotSegments},
		{name: "percent-encoded dot segments", domains: scoped, uri: "https://app.example.test/clients/%2e%2e/%2e%2e/evil", wantDesc: dotSegments},
		{name: "percent-encoded in upper case", domains: scoped, uri: "https://app.example.test/clients/%2E%2E/evil", wantDesc: dotSegments},
		{name: "half encoded", domains: scoped, uri: "https://app.example.test/clients/.%2e/evil", wantDesc: dotSegments},
		{name: "a trailing double dot", domains: scoped, uri: "https://app.example.test/clients/x/..", wantDesc: dotSegments},
		{name: "a single dot segment", domains: scoped, uri: "https://app.example.test/clients/./cb", wantDesc: dotSegments},
		{name: "separated by backslashes, which a browser reads as slashes", domains: scoped, uri: `https://app.example.test/clients/..\..\evil`, wantDesc: dotSegments},
		{name: "separated by encoded backslashes", domains: scoped, uri: "https://app.example.test/clients/..%5c..%5cevil", wantDesc: dotSegments},
		{name: "separated by encoded slashes", domains: scoped, uri: "https://app.example.test/clients/..%2f..%2fevil", wantDesc: dotSegments},
		{name: "under any domain", domains: []string{"*"}, uri: "https://app.example.test/a/../b", wantDesc: dotSegments},
		{name: "on a loopback callback", domains: []string{"http://localhost"}, uri: "http://127.0.0.1:8123/cb/../x", wantDesc: dotSegments},

		{name: "a sibling of the path the entry names", domains: scoped, uri: "https://app.example.test/clients-evil/cb", wantDesc: notPermitted},
		{name: "the parent of the path the entry names", domains: scoped, uri: "https://app.example.test/cb", wantDesc: notPermitted},

		{name: "an https loopback entry and the plain-http loopback address", domains: []string{"https://localhost"}, uri: "http://127.0.0.1:9000/cb", wantDesc: notPermitted},
		{name: "an https loopback entry and plain-http localhost", domains: []string{"https://localhost"}, uri: "http://localhost:9000/cb", wantDesc: notPermitted},
		{name: "an https loopback entry and the plain-http ipv6 loopback", domains: []string{"https://127.0.0.1/"}, uri: "http://[::1]:9000/cb", wantDesc: notPermitted},
		{name: "an https loopback entry in upper case", domains: []string{"HTTPS://LOCALHOST"}, uri: "http://127.0.0.1:9000/cb", wantDesc: notPermitted},
		{name: "a private-use scheme on a loopback host", domains: []string{"myapp://localhost"}, uri: "http://127.0.0.1:9000/cb", wantDesc: notPermitted},
		{name: "an https loopback entry and its own origin", domains: []string{"https://localhost"}, uri: "https://localhost/cb"},
		{name: "an https loopback entry with a port and that port", domains: []string{"https://localhost:8443"}, uri: "https://localhost:8443/cb"},
		{name: "an https loopback entry with a port and another port", domains: []string{"https://localhost:8443"}, uri: "https://localhost:9443/cb", wantDesc: notPermitted},
		{name: "an http loopback entry and any port", domains: []string{"http://localhost"}, uri: "http://127.0.0.1:53821/callback"},
		{name: "an http loopback entry in upper case", domains: []string{"HTTP://LOCALHOST/"}, uri: "http://127.0.0.1:53821/callback"},
		{name: "a loopback entry with no scheme", domains: []string{"localhost"}, uri: "http://127.0.0.1:53821/callback"},
		{name: "an http loopback entry with a port and another port", domains: []string{"http://localhost:3000"}, uri: "http://localhost:4000/cb", wantDesc: notPermitted},
		{name: "one https entry beside an http loopback entry", domains: []string{"https://localhost", "http://127.0.0.1"}, uri: "http://localhost:9000/cb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			body, err := json.Marshal(map[string]any{"redirect_uris": []string{tt.uri}})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			w := register(t, Config{Clients: store, RedirectDomains: tt.domains, CustomSchemes: []string{"myapp"}}, string(body))

			if tt.wantDesc == "" {
				if w.Code != http.StatusCreated {
					t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
				}
				regs := store.registrations()
				if len(regs) != 1 || len(regs[0].RedirectURIs) != 1 || regs[0].RedirectURIs[0] != tt.uri {
					t.Fatalf("store received %#v, want the single uri %q", regs, tt.uri)
				}
				return
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			var got registrationError
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if got.Error != errInvalidRedirectURI || got.ErrorDescription != tt.wantDesc {
				t.Fatalf("body = %#v, want %q / %q", got, errInvalidRedirectURI, tt.wantDesc)
			}
			if n := len(store.registrations()); n != 0 {
				t.Fatalf("store saw %d registrations, want 0", n)
			}
		})
	}
}

// A store failure is the application's problem, never the caller's to read.
func TestRegister_StoreFailuresStayServerSide(t *testing.T) {
	tests := []struct {
		name  string
		store *stubStore
	}{
		{"the store returns an error", &stubStore{failWith: errors.New("pq: connection to 10.0.0.7 refused, password=hunter2")}},
		{"the store returns no identifier", &stubStore{useAnswer: true, answer: RegisteredClient{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := register(t, openConfig(tt.store), `{"redirect_uris":["https://app.example.test/cb"]}`)

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
			}
			var got registrationError
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			want := registrationError{Error: errServerError, ErrorDescription: "The client could not be registered."}
			if got != want {
				t.Fatalf("body = %#v, want %#v", got, want)
			}
			if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "10.0.0.7") {
				t.Fatalf("body = %s, want no internal detail", w.Body.String())
			}
		})
	}
}

// paddedBody builds a registration document of exactly total bytes that every
// validation rule accepts. The padding sits in a member the rules say nothing
// about, so the only thing that can reject the document is its size: a test
// built on it fails the moment the route stops carrying the body limit.
func paddedBody(t *testing.T, total int) string {
	t.Helper()
	const (
		prefix = `{"redirect_uris":["https://app.example.test/cb"],"padding":"`
		suffix = `"}`
	)
	fill := total - len(prefix) - len(suffix)
	if fill < 0 {
		t.Fatalf("cannot build a %d byte document: the envelope alone is %d", total, len(prefix)+len(suffix))
	}
	body := prefix + strings.Repeat("a", fill) + suffix
	if len(body) != total {
		t.Fatalf("built a %d byte document, want %d", len(body), total)
	}
	return body
}

// The endpoint is unauthenticated, so the body has to be bounded, and the bound
// has to be the router's: it must stop the read rather than let the handler
// buffer the whole thing first.
func TestRegister_BodyLimitBoundary(t *testing.T) {
	tests := []struct {
		name       string
		size       int
		wantStatus int
		wantErr    string
		wantDesc   string
		wantSaved  int
	}{
		{
			name:       "exactly at the cap is accepted",
			size:       int(MaxRegistrationBodyBytes),
			wantStatus: http.StatusCreated,
			wantSaved:  1,
		},
		{
			name:       "one byte past the cap is refused",
			size:       int(MaxRegistrationBodyBytes) + 1,
			wantStatus: http.StatusBadRequest,
			wantErr:    errInvalidClientMetadata,
			// The document is rejected because it could not be read to the end,
			// not because anything in it was wrong: nothing in it is.
			wantDesc:  "The registration request body could not be parsed.",
			wantSaved: 0,
		},
		{
			name:       "far past the cap is refused the same way",
			size:       int(MaxRegistrationBodyBytes) * 4,
			wantStatus: http.StatusBadRequest,
			wantErr:    errInvalidClientMetadata,
			wantDesc:   "The registration request body could not be parsed.",
			wantSaved:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			w := register(t, openConfig(store), paddedBody(t, tt.size))

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantErr != "" {
				var got registrationError
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode %q: %v", w.Body.String(), err)
				}
				if got.Error != tt.wantErr || got.ErrorDescription != tt.wantDesc {
					t.Fatalf("body = %#v, want %q / %q", got, tt.wantErr, tt.wantDesc)
				}
			}
			if n := len(store.registrations()); n != tt.wantSaved {
				t.Fatalf("store saw %d registrations, want %d", n, tt.wantSaved)
			}
		})
	}
}

// A name is stored as the client sent it, in whatever script it is written: the
// endpoint is not in the business of rewriting a client's own name. That covers
// the two joiners, which are invisible themselves but decide how the visible
// characters next to them are drawn.
func TestRegister_ClientNameIsPreservedAsSent(t *testing.T) {
	tests := []struct {
		name       string
		clientName string
	}{
		{name: "latin with diacritics and a symbol", clientName: "Wetter\u00fcbersicht \u2600\ufe0f"},
		{name: "cjk", clientName: "\u5929\u6c17\u4e88\u5831\u30af\u30e9\u30a4\u30a2\u30f3\u30c8"},
		{name: "right-to-left script", clientName: "\u0639\u0645\u064a\u0644 \u0627\u0644\u0637\u0642\u0633"},
		{name: "an emoji sequence held together by a joiner", clientName: "Team \U0001f468\u200d\U0001f469\u200d\U0001f467"},
		{name: "a word that needs a non-joiner", clientName: "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645"},
		{name: "punctuation and spaces", clientName: "Acme (Staging) - v2.1 & Co."},
		{name: "exactly at the length cap", clientName: strings.Repeat("a", maxClientNameLength)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			body, err := json.Marshal(map[string]any{
				"client_name":   tt.clientName,
				"redirect_uris": []string{"https://app.example.test/cb"},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			w := register(t, openConfig(store), string(body))
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
			}
			regs := store.registrations()
			if len(regs) != 1 || regs[0].Name != tt.clientName {
				t.Fatalf("registrations = %+v, want one named %q", regs, tt.clientName)
			}
			var got registrationResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if got.ClientName != tt.clientName {
				t.Fatalf("client_name = %q, want %q", got.ClientName, tt.clientName)
			}
		})
	}
}

// The display name is what a user is shown when asked to trust the client, and
// an anonymous caller wrote it. A character nobody can see makes the name on the
// screen differ from the name on record: an override that reverses the letters
// after it, a line break that starts a second line of the caller's choosing, a
// zero-width or tag character that hides text. A NUL is refused by some stores
// outright. None of them reaches the store or comes back in the response, under
// either spelling of the member.
func TestRegister_RefusesANameThatDoesNotDisplayAsStored(t *testing.T) {
	characters := []struct {
		name string
		char string
	}{
		{name: "nul", char: "\x00"},
		{name: "carriage return", char: "\r"},
		{name: "line feed", char: "\n"},
		{name: "tab", char: "\t"},
		{name: "escape", char: "\x1b"},
		{name: "delete", char: "\x7f"},
		{name: "a c1 control", char: "\u0085"},
		{name: "line separator", char: "\u2028"},
		{name: "paragraph separator", char: "\u2029"},
		{name: "right-to-left override", char: "\u202e"},
		{name: "left-to-right embedding", char: "\u202a"},
		{name: "pop directional formatting", char: "\u202c"},
		{name: "right-to-left isolate", char: "\u2067"},
		{name: "pop directional isolate", char: "\u2069"},
		{name: "right-to-left mark", char: "\u200f"},
		{name: "arabic letter mark", char: "\u061c"},
		{name: "zero width space", char: "\u200b"},
		{name: "word joiner", char: "\u2060"},
		{name: "byte order mark", char: "\ufeff"},
		{name: "soft hyphen", char: "\u00ad"},
		{name: "a tag character", char: "\U000e0041"},
		{name: "interlinear annotation anchor", char: "\ufff9"},
	}
	positions := []struct {
		name  string
		place func(char string) string
	}{
		{name: "inside", place: func(char string) string { return "Trusted" + char + "App" }},
		{name: "leading", place: func(char string) string { return char + "Trusted App" }},
		{name: "trailing", place: func(char string) string { return "Trusted App" + char }},
	}

	for _, member := range []string{"client_name", "name"} {
		for _, character := range characters {
			for _, position := range positions {
				t.Run(member+"/"+character.name+"/"+position.name, func(t *testing.T) {
					store := &stubStore{id: "client-1"}
					body, err := json.Marshal(map[string]any{
						member:          position.place(character.char),
						"redirect_uris": []string{"https://app.example.test/cb"},
					})
					if err != nil {
						t.Fatalf("marshal: %v", err)
					}

					w := register(t, openConfig(store), string(body))

					if w.Code != http.StatusBadRequest {
						t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
					}
					var got registrationError
					if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
						t.Fatalf("decode %q: %v", w.Body.String(), err)
					}
					want := "The " + member + " field must not contain control or invisible formatting characters."
					if got.Error != errInvalidClientMetadata || got.ErrorDescription != want {
						t.Fatalf("body = %#v, want %q / %q", got, errInvalidClientMetadata, want)
					}
					if n := len(store.registrations()); n != 0 {
						t.Fatalf("store saw %d registrations, want 0", n)
					}
					if strings.Contains(got.ErrorDescription, character.char) {
						t.Fatalf("the refusal repeats the character it refused: %q", got.ErrorDescription)
					}
				})
			}
		}
	}
}

// The same holds for the URLs a client registers. A redirect URI is shown to the
// user being asked where the authorization response may go, the first one's host
// becomes the display name of a client that sent none, and a URI (RFC 3986) has
// no place for a raw invisible character to begin with.
func TestRegister_RefusesURLsThatDoNotDisplayAsStored(t *testing.T) {
	characters := []struct {
		name string
		char string
	}{
		{name: "right-to-left override", char: "\u202e"},
		{name: "right-to-left isolate", char: "\u2067"},
		{name: "zero width space", char: "\u200b"},
		{name: "line separator", char: "\u2028"},
		{name: "a tag character", char: "\U000e0041"},
		{name: "a c1 control", char: "\u0085"},
		{name: "byte order mark", char: "\ufeff"},
	}
	members := []struct {
		name     string
		payload  func(char string) map[string]any
		wantErr  string
		wantDesc string
	}{
		{
			name: "the host of a redirect uri",
			payload: func(char string) map[string]any {
				return map[string]any{"redirect_uris": []string{"https://app" + char + ".example.test/cb"}}
			},
			wantErr:  errInvalidRedirectURI,
			wantDesc: "redirect_uris.0 is not a valid URL.",
		},
		{
			name: "the path of a later redirect uri",
			payload: func(char string) map[string]any {
				return map[string]any{"redirect_uris": []string{"https://app.example.test/cb", "https://app.example.test/" + char + "cb"}}
			},
			wantErr:  errInvalidRedirectURI,
			wantDesc: "redirect_uris.1 is not a valid URL.",
		},
		{
			name: "the logo uri",
			payload: func(char string) map[string]any {
				return map[string]any{"redirect_uris": []string{"https://app.example.test/cb"}, "logo_uri": "https://app.example.test/" + char + "logo.png"}
			},
			wantErr:  errInvalidClientMetadata,
			wantDesc: "The logo_uri field must not contain control or invisible formatting characters.",
		},
		{
			name: "the client uri",
			payload: func(char string) map[string]any {
				return map[string]any{"redirect_uris": []string{"https://app.example.test/cb"}, "client_uri": "https://app" + char + ".example.test"}
			},
			wantErr:  errInvalidClientMetadata,
			wantDesc: "The client_uri field must not contain control or invisible formatting characters.",
		},
	}

	for _, member := range members {
		for _, character := range characters {
			t.Run(member.name+"/"+character.name, func(t *testing.T) {
				store := &stubStore{id: "client-1"}
				body, err := json.Marshal(member.payload(character.char))
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}

				w := register(t, openConfig(store), string(body))

				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
				}
				var got registrationError
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode %q: %v", w.Body.String(), err)
				}
				if got.Error != member.wantErr || got.ErrorDescription != member.wantDesc {
					t.Fatalf("body = %#v, want %q / %q", got, member.wantErr, member.wantDesc)
				}
				if n := len(store.registrations()); n != 0 {
					t.Fatalf("store saw %d registrations, want 0", n)
				}
			})
		}
	}
}

// The store is promised bounded input, and a list is only bounded when both its
// length and the length of each entry are. A 64 KiB body otherwise carries a
// couple of thousand redirect URIs, or one URI sixty thousand bytes long.
func TestRegister_BoundsTheRedirectURIsItHandsTheStore(t *testing.T) {
	uris := func(n int) []string {
		list := make([]string, n)
		for i := range list {
			list[i] = "https://app.example.test/cb/" + strconv.Itoa(i)
		}
		return list
	}
	ofLength := func(n int) string {
		const prefix = "https://app.example.test/"
		return prefix + strings.Repeat("a", n-len(prefix))
	}

	tests := []struct {
		name       string
		uris       []string
		wantStatus int
		wantErr    string
		wantDesc   string
	}{
		{name: "exactly as many as allowed", uris: uris(32), wantStatus: http.StatusCreated},
		{
			name:       "one more than allowed",
			uris:       uris(33),
			wantStatus: http.StatusBadRequest,
			wantErr:    errInvalidRedirectURI,
			wantDesc:   "The redirect_uris field must not have more than 32 items.",
		},
		{
			name:       "as many as the body cap lets through",
			uris:       uris(1500),
			wantStatus: http.StatusBadRequest,
			wantErr:    errInvalidRedirectURI,
			wantDesc:   "The redirect_uris field must not have more than 32 items.",
		},
		{name: "a uri exactly as long as allowed", uris: []string{ofLength(2048)}, wantStatus: http.StatusCreated},
		{
			name:       "a uri one byte longer than allowed",
			uris:       []string{ofLength(2049)},
			wantStatus: http.StatusBadRequest,
			wantErr:    errInvalidRedirectURI,
			wantDesc:   "redirect_uris.0 must not exceed 2048 characters.",
		},
		{
			name:       "a uri as long as the body cap lets through",
			uris:       []string{"https://app.example.test/cb", ofLength(60000)},
			wantStatus: http.StatusBadRequest,
			wantErr:    errInvalidRedirectURI,
			wantDesc:   "redirect_uris.1 must not exceed 2048 characters.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			body, err := json.Marshal(map[string]any{"redirect_uris": tt.uris})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if int64(len(body)) > MaxRegistrationBodyBytes {
				t.Fatalf("the body is %d bytes, past the cap this test means to stay under", len(body))
			}

			w := register(t, openConfig(store), string(body))

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %.200s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus == http.StatusCreated {
				regs := store.registrations()
				if len(regs) != 1 || len(regs[0].RedirectURIs) != len(tt.uris) {
					t.Fatalf("store saw %+v, want one registration with %d uris", regs, len(tt.uris))
				}
				return
			}
			var got registrationError
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if got.Error != tt.wantErr || got.ErrorDescription != tt.wantDesc {
				t.Fatalf("body = %#v, want %q / %q", got, tt.wantErr, tt.wantDesc)
			}
			if n := len(store.registrations()); n != 0 {
				t.Fatalf("store saw %d registrations, want 0", n)
			}
		})
	}
}

// One endpoint serves every client that discovers the server at once.
func TestRegister_Concurrent(t *testing.T) {
	store := &stubStore{id: "client-1"}
	r := mount(openConfig(store))

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, registrationRequest("/oauth/register", `{"redirect_uris":["https://app.example.test/cb"]}`))
			if w.Code != http.StatusCreated {
				t.Errorf("status = %d, want 201: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if n := len(store.registrations()); n != 40 {
		t.Fatalf("store saw %d registrations, want 40", n)
	}
}

// The two postures the fuzz target drives every body through: any http(s)
// origin plus one private-use scheme, which leaves only the limits the endpoint
// enforces on its own, and a single named origin, which is where a hole in the
// allowlist would show.
const (
	fuzzScheme      = "myapp"
	fuzzAllowedHost = "app.example.test"
	fuzzAllowedURL  = "https://" + fuzzAllowedHost
)

// A registration request is one JSON object (RFC 7591 3.1). A body that
// carries more than that is not the request its sender wrote, so the endpoint
// must refuse it instead of provisioning a client from the part it understood.
func TestRegister_RejectsDataAfterTheRegistrationDocument(t *testing.T) {
	const document = `{"redirect_uris":["https://app.example.test/cb"]}`

	tests := []struct {
		name   string
		body   string
		accept bool
	}{
		{name: "a second registration document", body: document + document},
		{name: "a second document asking for another callback", body: document + `{"redirect_uris":["https://evil.test/cb"]}`},
		{name: "a trailing json value", body: document + ` null`},
		{name: "trailing garbage", body: document + " not json at all"},
		// The tokens that close a value the decoder is not inside: it reports
		// no more input for them, so they are the ones a trailing-data check
		// built on that question would wave through.
		{name: "a trailing closing bracket", body: document + "]"},
		{name: "a trailing closing brace", body: document + "}"},
		{name: "a truncated second document", body: document + `{"redirect_uris":[`},
		// Whitespace is not data: a body ending in a newline is the same
		// request.
		{name: "trailing whitespace", body: document + " \r\n\t", accept: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &stubStore{id: "client-1"}
			w := register(t, openConfig(store), tt.body)

			if tt.accept {
				if w.Code != http.StatusCreated {
					t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
				}
				if n := len(store.registrations()); n != 1 {
					t.Fatalf("store saw %d registrations, want 1", n)
				}
				return
			}

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			var got registrationError
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", w.Body.String(), err)
			}
			if got.Error != errInvalidClientMetadata {
				t.Errorf("error = %q, want %q", got.Error, errInvalidClientMetadata)
			}
			if want := "The registration request body could not be parsed."; got.ErrorDescription != want {
				t.Errorf("error_description = %q, want %q", got.ErrorDescription, want)
			}
			if n := len(store.registrations()); n != 0 {
				t.Errorf("store saw %d registrations, want none", n)
			}
		})
	}
}

// FuzzRegistrationBody drives arbitrary bodies through the endpoint. It sits in
// front of an unauthenticated route, so it must never panic and must only ever
// answer with one of the outcomes RFC 7591 3.2 defines.
//
// What the oracle asks of an accepted registration is not the policy's own
// reasoning restated but what the specification requires of the result: the
// store was handed exactly the callbacks the body asked for and no others, a
// plaintext callback goes nowhere but the loopback interface (RFC 8252 7.3), no
// callback carries a fragment (RFC 6749 3.1.2), no callback uses a scheme the
// application never named, and under a named allowlist the authorization
// response can reach that one host and nothing else. The per-URI verdicts
// themselves are pinned by the tables above, which state them independently.
func FuzzRegistrationBody(f *testing.F) {
	f.Add(`{"redirect_uris":["https://app.example.test/cb"]}`)
	f.Add(`{"redirect_uris":[42]}`)
	f.Add(`{"redirect_uris":["myapp://cb"],"client_name":"x"}`)
	f.Add(`{"redirect_uris":{"0":"https://app.example.test/cb"}}`)
	f.Add("{" + string(rune(34)) + "redirect_uris" + string(rune(34)) + ":[" + string(rune(34)) + "https://" + string(rune(0)) + ".test/cb" + string(rune(34)) + "]}")
	f.Add(`{"logo_uri":"https://app.example.test/l.png"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb#frag"]}`)
	f.Add(`{"redirect_uris":["myapp:opaque"]}`)
	f.Add(`{"redirect_uris":["myapp:/oauth2redirect/provider"]}`)
	f.Add(`{"redirect_uris":["http://public.example/cb"]}`)
	f.Add(`{"redirect_uris":["http://127.0.0.1:8080/cb"]}`)
	f.Add(`{"redirect_uris":["myapp:///oauth2redirect/provider"]}`)
	f.Add(`{"redirect_uris":["myapp://user@/cb"]}`)
	f.Add(`{"redirect_uris":["myapp://:8080/cb"]}`)
	f.Add(`{"redirect_uris":["https://:8443/cb"]}`)
	f.Add(`{"redirect_uris":["MYAPP://cb"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test@evil.test/cb"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test.evil.test/cb"]}`)
	f.Add(`{"redirect_uris":["http://app.example.test@localhost/cb"]}`)
	f.Add(`{"redirect_uris":["https://APP.EXAMPLE.TEST/cb"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb","ftp://app.example.test/cb"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test/a/../../b"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test/a/%2e%2e/b"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test/a/..\\..\\b"]}`)
	f.Add(`{"redirect_uris":["https://app\u202e.example.test/cb"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"],"logo_uri":"https://app.example.test/\u200blogo.png"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"],"client_name":"Trusted\u202eApp"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"],"client_name":"a\u0000b"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"],"name":"a\r\nb"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"],"client_name":"a\udb40\udc41b"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"],"client_name":"` + strings.Repeat("n", 300) + `"}`)
	f.Add(`{"redirect_uris":["https://app.example.test/` + strings.Repeat("a", 3000) + `"]}`)
	f.Add(`{"redirect_uris":[` + strings.Repeat(`"https://app.example.test/cb",`, 40) + `"https://app.example.test/cb"]}`)
	f.Add(`[]`)
	f.Add(`null`)
	f.Add(``)
	f.Add(`{`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"]}{"redirect_uris":["https://evil.test/cb"]}`)
	f.Add(`{"redirect_uris":["https://app.example.test/cb"]}]`)

	openStore := &stubStore{id: "client-open"}
	openCfg := openConfig(openStore)
	openCfg.CustomSchemes = []string{fuzzScheme}

	namedStore := &stubStore{id: "client-named"}
	namedCfg := Config{BaseURL: "https://mcp.example.test", Clients: namedStore, RedirectDomains: []string{fuzzAllowedURL}}

	postures := []struct {
		name   string
		store  *stubStore
		router http.Handler
		// schemes are the schemes this posture could ever deliver over.
		schemes []string
		// host, when set, is the only host an accepted callback may name.
		host string
	}{
		{name: "any origin", store: openStore, router: mount(openCfg), schemes: []string{"http", "https", fuzzScheme}},
		{name: "one named origin", store: namedStore, router: mount(namedCfg), schemes: []string{"https"}, host: fuzzAllowedHost},
	}

	f.Fuzz(func(t *testing.T, body string) {
		asked := requestedRedirectURIs(body)

		for _, posture := range postures {
			before := len(posture.store.registrations())

			w := httptest.NewRecorder()
			posture.router.ServeHTTP(w, registrationRequest("/oauth/register", body))

			switch w.Code {
			case http.StatusCreated, http.StatusBadRequest, http.StatusInternalServerError:
			default:
				t.Fatalf("%s: status = %d for body %q, want 201, 400, or 500", posture.name, w.Code, body)
			}

			if w.Code != http.StatusCreated {
				var got registrationError
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("%s: error body %q is not a registration error: %v", posture.name, w.Body.String(), err)
				}
				// RFC 7591 3.2.2 names the codes a registration request may be
				// refused with; anything else is not an answer a client can read.
				switch got.Error {
				case errInvalidRedirectURI, errInvalidClientMetadata, errServerError:
				default:
					t.Fatalf("%s: error code = %q for body %q, outside the registration error codes", posture.name, got.Error, body)
				}
				if len(posture.store.registrations()) != before {
					t.Fatalf("%s: body %q was refused with %d but still reached the store", posture.name, body, w.Code)
				}
				continue
			}

			saved := posture.store.registrations()
			if len(saved) != before+1 {
				t.Fatalf("%s: body %q answered 201 but the store recorded %d new clients", posture.name, body, len(saved)-before)
			}
			reg := saved[len(saved)-1]
			if reg.Name == "" {
				t.Fatalf("%s: body %q registered a client with no name", posture.name, body)
			}
			// The store is promised bounded input it can show as it stands.
			if len(reg.Name) > 255 {
				t.Fatalf("%s: body %q registered a %d byte name", posture.name, body, len(reg.Name))
			}
			for _, r := range reg.Name {
				if r != '\u200c' && r != '\u200d' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)) {
					t.Fatalf("%s: body %q registered the name %q, which holds the undisplayable %U", posture.name, body, reg.Name, r)
				}
			}
			if len(reg.RedirectURIs) > 32 {
				t.Fatalf("%s: body %q registered %d redirect uris", posture.name, body, len(reg.RedirectURIs))
			}
			for _, bounded := range append([]string{reg.LogoURI, reg.ClientURI}, reg.RedirectURIs...) {
				if len(bounded) > 2048 {
					t.Fatalf("%s: body %q handed the store a %d byte url", posture.name, body, len(bounded))
				}
				for _, r := range bounded {
					if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
						t.Fatalf("%s: body %q handed the store the url %q, which holds the undisplayable %U", posture.name, body, bounded, r)
					}
				}
			}
			// RFC 7591 3.2.1 has the response report the metadata the client is
			// registered under, including what this server provisioned for it.
			var info registrationResponse
			if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
				t.Fatalf("%s: response %q is not a client information response: %v", posture.name, w.Body.String(), err)
			}
			if info.ClientName != reg.Name {
				t.Fatalf("%s: body %q was registered as %q but reported client_name %q", posture.name, body, reg.Name, info.ClientName)
			}
			if len(reg.RedirectURIs) == 0 {
				t.Fatalf("%s: body %q registered a client with no redirect uri", posture.name, body)
			}

			for _, uri := range reg.RedirectURIs {
				if !slices.Contains(asked, uri) {
					t.Fatalf("%s: body %q registered redirect uri %q, which it never asked for", posture.name, body, uri)
				}
				parsed, err := url.Parse(uri)
				if err != nil {
					t.Fatalf("%s: body %q registered an unparseable redirect uri %q: %v", posture.name, body, uri, err)
				}
				scheme := strings.ToLower(parsed.Scheme)
				if !slices.Contains(posture.schemes, scheme) {
					t.Fatalf("%s: body %q registered redirect uri %q on scheme %q, which this configuration never named", posture.name, body, uri, scheme)
				}
				if parsed.Fragment != "" || strings.HasSuffix(uri, "#") {
					t.Fatalf("%s: body %q registered redirect uri %q carrying a fragment", posture.name, body, uri)
				}
				// A callback is delivered where it says, not somewhere its dot
				// segments resolve to (RFC 3986 5.2.4).
				for _, segment := range strings.Split(strings.ReplaceAll(parsed.Path, `\`, "/"), "/") {
					if segment == "." || segment == ".." {
						t.Fatalf("%s: body %q registered redirect uri %q carrying a dot segment", posture.name, body, uri)
					}
				}
				// The authorization response travels back over this URI, so a
				// plaintext one may only be delivered to this machine.
				if scheme == "http" && !onLoopback(parsed.Hostname()) {
					t.Fatalf("%s: body %q registered plain-http redirect uri %q to a host that is not loopback", posture.name, body, uri)
				}
				if posture.host != "" && !strings.EqualFold(parsed.Hostname(), posture.host) {
					t.Fatalf("%s: body %q registered redirect uri %q, whose host is not the one permitted origin %q", posture.name, body, uri, posture.host)
				}
			}
		}
	})
}

// requestedRedirectURIs returns the redirect URIs a registration body literally
// asked for, so the oracle can tell a callback the client named from one the
// endpoint invented. A body the endpoint could not accept yields nothing.
func requestedRedirectURIs(body string) []string {
	var payload struct {
		RedirectURIs []any `json:"redirect_uris"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return nil
	}
	uris := make([]string, 0, len(payload.RedirectURIs))
	for _, item := range payload.RedirectURIs {
		if uri, ok := item.(string); ok {
			uris = append(uris, uri)
		}
	}
	return uris
}

// onLoopback reports whether a plaintext callback to host stays on this
// machine. It asks the network stack what the address is rather than compare
// against a list, so the oracle does not inherit the endpoint's reading of
// which hosts count as loopback.
func onLoopback(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// sameResponse compares two registration responses field by field.
func sameResponse(got, want registrationResponse) bool {
	return got.ClientID == want.ClientID &&
		got.ClientName == want.ClientName &&
		got.Scope == want.Scope &&
		got.TokenEndpointAuthMethod == want.TokenEndpointAuthMethod &&
		got.LogoURI == want.LogoURI &&
		got.ClientURI == want.ClientURI &&
		sameStrings(got.GrantTypes, want.GrantTypes) &&
		sameStrings(got.ResponseTypes, want.ResponseTypes) &&
		sameStrings(got.RedirectURIs, want.RedirectURIs)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The registration endpoint is mounted under a stable name so an application
// can find, replace, or wrap it.
func TestRegister_RouteIsNamed(t *testing.T) {
	r := mount(openConfig(&stubStore{id: "client-1"}))

	for _, info := range r.AllRoutes() {
		if info.Method == http.MethodPost && info.Path == "/oauth/register" {
			if info.Name != RouteRegister {
				t.Fatalf("route name = %q, want %q", info.Name, RouteRegister)
			}
			return
		}
	}
	t.Fatalf("POST /oauth/register is not registered")
}
