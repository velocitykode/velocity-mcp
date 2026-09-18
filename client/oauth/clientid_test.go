package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// metadataServer is a fake authorization server whose advertised metadata the
// test controls, counting the registration and token calls it receives.
type metadataServer struct {
	srv          *httptest.Server
	registered   atomic.Int64
	tokenForms   chan url.Values
	tokenAuth    chan string
	discoveries  atomic.Int64
	registerBody chan map[string]any
}

// newMetadataServer starts a fake authorization server. extra is merged into the
// advertised authorization-server metadata, so a test can add or remove fields
// such as code_challenge_methods_supported.
func newMetadataServer(t *testing.T, extra map[string]any) *metadataServer {
	t.Helper()
	m := &metadataServer{
		tokenForms:   make(chan url.Values, 8),
		tokenAuth:    make(chan string, 8),
		registerBody: make(chan map[string]any, 8),
	}
	mux := http.NewServeMux()
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              m.srv.URL,
			"authorization_servers": []string{m.srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		m.discoveries.Add(1)
		doc := map[string]any{
			"issuer":                           m.srv.URL,
			"authorization_endpoint":           m.srv.URL + "/authorize",
			"token_endpoint":                   m.srv.URL + "/token",
			"registration_endpoint":            m.srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		}
		for k, v := range extra {
			if v == nil {
				delete(doc, k)
				continue
			}
			doc[k] = v
		}
		writeJSON(w, doc)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		n := m.registered.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case m.registerBody <- body:
		default:
		}
		writeJSON(w, map[string]any{
			"client_id":     "dyn-client",
			"client_secret": "dyn-secret",
			"sequence":      n,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		select {
		case m.tokenForms <- r.Form:
			m.tokenAuth <- r.Header.Get("Authorization")
		default:
		}
		writeJSON(w, map[string]any{"access_token": "access-123", "token_type": "Bearer", "expires_in": 3600})
	})
	return m
}

// authorizeQuery drives one authorization start and returns the query of the
// resulting authorization URL.
func authorizeQuery(t *testing.T, c *Client) url.Values {
	t.Helper()
	authURL, _, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorization url %q: %v", authURL, err)
	}
	return u.Query()
}

func TestAuthorizationURLRequiresAdvertisedPKCE(t *testing.T) {
	const documentURL = "https://app.example.com/mcp/oauth/github/client-metadata.json"

	tests := []struct {
		name            string
		methods         any // nil removes code_challenge_methods_supported
		wantPKCERefusal bool
	}{
		{name: "field omitted", methods: nil, wantPKCERefusal: true},
		{name: "empty list", methods: []string{}, wantPKCERefusal: true},
		{name: "plain only", methods: []string{"plain"}, wantPKCERefusal: true},
		{name: "wrong case", methods: []string{"s256"}, wantPKCERefusal: true},
		{name: "non string entries", methods: []any{1, true}, wantPKCERefusal: true},
		{name: "s256 advertised", methods: []string{"S256"}, wantPKCERefusal: false},
		{name: "s256 among others", methods: []string{"plain", "S256"}, wantPKCERefusal: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{
				"code_challenge_methods_supported":      tt.methods,
				"client_id_metadata_document_supported": true,
			})
			c := NewClient(Config{
				RedirectURI:         "https://app.example.com/callback",
				ClientIDMetadataURL: documentURL,
			}, as.srv.URL, "", "")

			authURL, pending, err := c.AuthorizationURL(context.Background(), "")
			if !tt.wantPKCERefusal {
				if err != nil {
					t.Fatalf("authorization url: %v", err)
				}
				if pending.Verifier == "" {
					t.Fatal("expected a PKCE verifier in the pending authorization")
				}
				u, _ := url.Parse(authURL)
				if got := u.Query().Get("code_challenge_method"); got != "S256" {
					t.Fatalf("code_challenge_method = %q, want S256", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected refusal, got authorization url %q", authURL)
			}
			if !errors.Is(err, ErrPKCERequired) {
				t.Fatalf("error %v does not wrap ErrPKCERequired", err)
			}
			if pending != nil {
				t.Fatalf("pending authorization must not be returned: %+v", pending)
			}
			// The refusal happens before the client identifies itself, so no
			// client record is created on the server.
			if n := as.registered.Load(); n != 0 {
				t.Fatalf("registration requests = %d, want 0", n)
			}
		})
	}
}

func TestAuthorizationURLPKCERefusalMessages(t *testing.T) {
	tests := []struct {
		name    string
		methods any
		want    string
	}{
		{name: "omitted names the missing field", methods: nil, want: "code_challenge_methods_supported"},
		{name: "unsupported names the method", methods: []string{"plain"}, want: "S256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{"code_challenge_methods_supported": tt.methods})
			c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL, "", "")
			_, _, err := c.AuthorizationURL(context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestClientIDResolutionOrder(t *testing.T) {
	const documentURL = "https://app.example.com/mcp/oauth/github/client-metadata.json"

	tests := []struct {
		name            string
		documentSupport bool
		config          Config
		wantClientID    string
		wantSecret      string
		wantAuthMethod  string
		wantRegistered  int64
	}{
		{
			name:            "configured client id wins over the metadata document",
			documentSupport: true,
			config:          Config{ClientID: "client-123", ClientSecret: "shh", ClientIDMetadataURL: documentURL},
			wantClientID:    "client-123",
			wantSecret:      "shh",
			wantAuthMethod:  authMethodPost,
		},
		{
			name:            "metadata document wins over dynamic registration",
			documentSupport: true,
			config:          Config{ClientIDMetadataURL: documentURL},
			wantClientID:    documentURL,
			wantAuthMethod:  authMethodNone,
		},
		{
			name:            "configured secret is dropped for a metadata document client",
			documentSupport: true,
			config:          Config{ClientSecret: "leaked-secret", ClientIDMetadataURL: documentURL},
			wantClientID:    documentURL,
			wantAuthMethod:  authMethodNone,
		},
		{
			name:            "dynamic registration when the server does not support documents",
			documentSupport: false,
			config:          Config{ClientIDMetadataURL: documentURL},
			wantClientID:    "dyn-client",
			wantSecret:      "dyn-secret",
			wantAuthMethod:  authMethodPost,
			wantRegistered:  1,
		},
		{
			name:            "dynamic registration when no document url is configured",
			documentSupport: true,
			config:          Config{},
			wantClientID:    "dyn-client",
			wantSecret:      "dyn-secret",
			wantAuthMethod:  authMethodPost,
			wantRegistered:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{"client_id_metadata_document_supported": tt.documentSupport})
			cfg := tt.config
			cfg.RedirectURI = "https://app.example.com/callback"
			cfg.Issuer = as.srv.URL
			c := NewClient(cfg, as.srv.URL, "", "")

			authURL, pending, err := c.AuthorizationURL(context.Background(), "")
			if err != nil {
				t.Fatalf("authorization url: %v", err)
			}
			u, _ := url.Parse(authURL)
			if got := u.Query().Get("client_id"); got != tt.wantClientID {
				t.Fatalf("client_id in authorization url = %q, want %q", got, tt.wantClientID)
			}
			if pending.ClientID != tt.wantClientID {
				t.Fatalf("pending client id = %q, want %q", pending.ClientID, tt.wantClientID)
			}
			if pending.ClientSecret != tt.wantSecret {
				t.Fatalf("pending client secret = %q, want %q", pending.ClientSecret, tt.wantSecret)
			}
			if pending.TokenAuthMethod != tt.wantAuthMethod {
				t.Fatalf("token auth method = %q, want %q", pending.TokenAuthMethod, tt.wantAuthMethod)
			}
			if n := as.registered.Load(); n != tt.wantRegistered {
				t.Fatalf("registration requests = %d, want %d", n, tt.wantRegistered)
			}
		})
	}
}

func TestExchangeCodeForPublicClientSendsNoSecret(t *testing.T) {
	const documentURL = "https://app.example.com/mcp/oauth/github/client-metadata.json"
	as := newMetadataServer(t, map[string]any{"client_id_metadata_document_supported": true})
	c := NewClient(Config{
		ClientSecret:        "leaked-secret",
		RedirectURI:         "https://app.example.com/callback",
		ClientIDMetadataURL: documentURL,
	}, as.srv.URL, "", "")

	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}
	token, _, err := c.ExchangeCode(context.Background(), pending, url.Values{
		"code":  {"the-code"},
		"state": {pending.State},
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if token.ClientID != documentURL || token.ClientSecret != "" {
		t.Fatalf("token credentials = %q / %q, want %q / \"\"", token.ClientID, token.ClientSecret, documentURL)
	}

	form := <-as.tokenForms
	if form.Get("client_id") != documentURL {
		t.Fatalf("token request client_id = %q, want %q", form.Get("client_id"), documentURL)
	}
	if _, present := form["client_secret"]; present {
		t.Fatalf("token request leaked a client_secret: %v", form)
	}
}

func TestRefreshNeverPairsTheConfiguredSecretWithAnotherClient(t *testing.T) {
	const documentURL = "https://app.example.com/mcp/oauth/github/client-metadata.json"

	tests := []struct {
		name         string
		config       Config
		clientID     string
		clientSecret string
		wantID       string
		wantSecret   string
	}{
		{
			name:       "public client refreshes without a secret",
			config:     Config{ClientID: "client-123", ClientSecret: "configured"},
			clientID:   documentURL,
			wantID:     documentURL,
			wantSecret: "",
		},
		{
			name:       "configured credentials are used when nothing is passed",
			config:     Config{ClientID: "client-123", ClientSecret: "configured"},
			wantID:     "client-123",
			wantSecret: "configured",
		},
		{
			name:         "explicit credentials win",
			config:       Config{ClientID: "client-123", ClientSecret: "configured"},
			clientID:     "stored-client",
			clientSecret: "stored-secret",
			wantID:       "stored-client",
			wantSecret:   "stored-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, nil)
			cfg := tt.config
			cfg.Issuer = as.srv.URL
			c := NewClient(cfg, as.srv.URL, "", "")

			token, err := c.Refresh(context.Background(), &TokenSet{
				RefreshToken: "old-refresh",
				ClientID:     tt.clientID,
				ClientSecret: tt.clientSecret,
				Issuer:       as.srv.URL,
			})
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if token.ClientID != tt.wantID || token.ClientSecret != tt.wantSecret {
				t.Fatalf("token credentials = %q / %q, want %q / %q",
					token.ClientID, token.ClientSecret, tt.wantID, tt.wantSecret)
			}

			form := <-as.tokenForms
			if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" {
				t.Fatalf("token request = %v", form)
			}
			if form.Get("client_id") != tt.wantID {
				t.Fatalf("token request client_id = %q, want %q", form.Get("client_id"), tt.wantID)
			}
			if got := form.Get("client_secret"); got != tt.wantSecret {
				t.Fatalf("token request client_secret = %q, want %q", got, tt.wantSecret)
			}
		})
	}
}

func TestValidClientIDMetadataURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "https url with a path", url: "https://app.example.com/client-metadata.json", want: true},
		{name: "nested path", url: "https://app.example.com/mcp/oauth/github/client-metadata.json", want: true},
		{name: "explicit port", url: "https://app.example.com:8443/client-metadata.json", want: true},
		{name: "public ip address", url: "https://93.184.216.34/client-metadata.json", want: true},
		{name: "unicode host", url: "https://exämple.com/client-metadata.json", want: true},
		{name: "http scheme", url: "http://app.example.com/client-metadata.json", want: false},
		{name: "no scheme", url: "app.example.com/client-metadata.json", want: false},
		{name: "empty", url: "", want: false},
		{name: "no path component", url: "https://app.example.com", want: false},
		{name: "empty path component", url: "https://app.example.com/", want: false},
		{name: "fragment component", url: "https://app.example.com/client-metadata.json#part", want: false},
		// A fragment written as empty is still a fragment, and the identifier
		// the authorization server receives would differ from the one the
		// client thinks it sent.
		{name: "empty fragment", url: "https://app.example.com/client-metadata.json#", want: false},
		// The identifier is compared as written, so a path that only reaches
		// the document once dot segments are removed is not the identifier the
		// authorization server would dereference.
		{name: "double dot path segment", url: "https://app.example.com/a/../client-metadata.json", want: false},
		{name: "single dot path segment", url: "https://app.example.com/a/./client-metadata.json", want: false},
		{name: "leading double dot segment", url: "https://app.example.com/../client-metadata.json", want: false},
		{name: "trailing double dot segment", url: "https://app.example.com/client-metadata.json/..", want: false},
		{name: "percent encoded double dot segment", url: "https://app.example.com/a/%2e%2e/client-metadata.json", want: false},
		{name: "percent encoded single dot segment", url: "https://app.example.com/a/%2E/client-metadata.json", want: false},
		// A dot inside a segment is an ordinary character: only a segment that
		// is exactly one or two dots is a dot segment.
		{name: "leading dot in a segment", url: "https://app.example.com/.well-known/client-metadata.json", want: true},
		{name: "three dots in a segment", url: "https://app.example.com/.../client-metadata.json", want: true},
		{name: "dots inside a file name", url: "https://app.example.com/a..b/client-metadata.json", want: true},
		{name: "query component", url: "https://app.example.com/client-metadata.json?tenant=1", want: false},
		{name: "forced empty query", url: "https://app.example.com/client-metadata.json?", want: false},
		{name: "user info", url: "https://user:pass@app.example.com/client-metadata.json", want: false},
		{name: "no host", url: "https:///client-metadata.json", want: false},
		{name: "unparsable url", url: "https://exa mple.com/\x7f", want: false},
		{name: "control character", url: "https://app.example.com/client\nmetadata.json", want: false},
		{name: "localhost host", url: "https://localhost/client-metadata.json", want: false},
		{name: "loopback address", url: "https://127.0.0.1/client-metadata.json", want: false},
		{name: "ipv6 loopback", url: "https://[::1]/client-metadata.json", want: false},
		{name: "private network address", url: "https://192.168.1.40/client-metadata.json", want: false},
		{name: "link local address", url: "https://169.254.10.1/client-metadata.json", want: false},
		{name: "unspecified address", url: "https://0.0.0.0/client-metadata.json", want: false},
		// Special-purpose ranges are globally unicast as far as the standard
		// library is concerned, but no authorization server can fetch a
		// document from one of them.
		{name: "this network block", url: "https://0.1.2.3/client-metadata.json", want: false},
		{name: "carrier grade nat", url: "https://100.64.0.1/client-metadata.json", want: false},
		{name: "ietf protocol assignments", url: "https://192.0.0.8/client-metadata.json", want: false},
		{name: "test net 1", url: "https://192.0.2.1/client-metadata.json", want: false},
		{name: "6to4 relay anycast", url: "https://192.88.99.1/client-metadata.json", want: false},
		{name: "benchmarking range", url: "https://198.18.0.1/client-metadata.json", want: false},
		{name: "test net 2", url: "https://198.51.100.1/client-metadata.json", want: false},
		{name: "test net 3", url: "https://203.0.113.1/client-metadata.json", want: false},
		{name: "reserved class e", url: "https://240.0.0.1/client-metadata.json", want: false},
		{name: "broadcast address", url: "https://255.255.255.255/client-metadata.json", want: false},
		{name: "ipv6 documentation range", url: "https://[2001:db8::1]/client-metadata.json", want: false},
		{name: "ipv6 unique local", url: "https://[fd00::1]/client-metadata.json", want: false},
		{name: "ipv6 6to4", url: "https://[2002::1]/client-metadata.json", want: false},
		{name: "ipv6 public address", url: "https://[2606:4700::1111]/client-metadata.json", want: true},
		{name: "ipv4 mapped private address", url: "https://[::ffff:192.168.1.40]/client-metadata.json", want: false},
		{name: "test domain", url: "https://app.test/client-metadata.json", want: false},
		{name: "local domain", url: "https://app.local/client-metadata.json", want: false},
		{name: "internal domain", url: "https://app.internal/client-metadata.json", want: false},
		{name: "invalid domain", url: "https://app.invalid/client-metadata.json", want: false},
		{name: "example domain", url: "https://app.example/client-metadata.json", want: false},
		{name: "uppercase reserved domain", url: "https://APP.TEST/client-metadata.json", want: false},
		{name: "single label host", url: "https://intranet/client-metadata.json", want: false},
		// A trailing root dot names the same host, so it must not smuggle a
		// reserved name past the suffix check.
		{name: "fully qualified reserved domain", url: "https://app.test./client-metadata.json", want: false},
		{name: "fully qualified localhost", url: "https://localhost./client-metadata.json", want: false},
		{name: "fully qualified public domain", url: "https://app.example.com./client-metadata.json", want: true},
		{name: "root dot only", url: "https://./client-metadata.json", want: false},
		// A zone identifier scopes an address to one interface on one machine.
		// The standard library declines to parse the result as an address, so
		// without an explicit check it would pass as a dotted name.
		{name: "ipv6 zone identifier", url: "https://[fe80::1%25eth0]/client-metadata.json", want: false},
		{name: "ipv6 zone identifier spelled as a domain", url: "https://[2001:4860::1%25a.b]/client-metadata.json", want: false},
		// Non-canonical spellings of an IPv4 address are not addresses as far as
		// the standard library is concerned and not domains either: no top-level
		// domain is numeric or a hexadecimal literal.
		{name: "hexadecimal loopback", url: "https://0x7f.1/client-metadata.json", want: false},
		{name: "short form loopback", url: "https://127.1/client-metadata.json", want: false},
		{name: "octal loopback", url: "https://0177.0.0.1/client-metadata.json", want: false},
		{name: "numeric top label", url: "https://app.example.123/client-metadata.json", want: false},
		{name: "hexadecimal top label", url: "https://app.example.0xff/client-metadata.json", want: false},
		// A label that merely starts with a digit is an ordinary domain.
		{name: "top label beginning with a digit", url: "https://app.example.1com/client-metadata.json", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validClientIDMetadataURL(tt.url); got != tt.want {
				t.Fatalf("validClientIDMetadataURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestClientIDMetadataURLFallsBackToRegistration(t *testing.T) {
	// A document URL the authorization server could never fetch must not be
	// presented as a client_id; the flow registers dynamically instead.
	for _, documentURL := range []string{
		"http://app.example.com/client-metadata.json",
		"https://app.example.com",
		"https://localhost/client-metadata.json",
		"https://192.168.1.40/client-metadata.json",
		"https://app.test/client-metadata.json",
		"https:///client-metadata.json",
	} {
		t.Run(documentURL, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{"client_id_metadata_document_supported": true})
			c := NewClient(Config{
				RedirectURI:         "https://app.example.com/callback",
				ClientIDMetadataURL: documentURL,
			}, as.srv.URL, "", "")

			if got := authorizeQuery(t, c).Get("client_id"); got != "dyn-client" {
				t.Fatalf("client_id = %q, want the dynamically registered id", got)
			}
			if n := as.registered.Load(); n != 1 {
				t.Fatalf("registration requests = %d, want 1", n)
			}
		})
	}
}

// reachableFromTheInternet is the oracle the fuzz target checks accepted hosts
// against. It is written from the requirement (an authorization server has to
// be able to fetch the document over the public internet) rather than from the
// implementation, so a host the implementation lets through but no server could
// reach fails the target.
func reachableFromTheInternet(t *testing.T, host string) bool {
	t.Helper()
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" || host == "localhost" {
		return false
	}
	// An address scoped to one machine's interface names nothing anybody else
	// can reach.
	if strings.Contains(host, "%") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return false
		}
		for _, block := range []string{
			"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
			"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
			"240.0.0.0/4", "64:ff9b:1::/48", "100::/64", "2001::/23",
			"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16",
		} {
			_, n, err := net.ParseCIDR(block)
			if err != nil {
				t.Fatalf("test oracle has a malformed block %q: %v", block, err)
			}
			if n.Contains(ip) {
				return false
			}
		}
		return true
	}
	for _, suffix := range []string{".test", ".local", ".localhost", ".internal", ".invalid", ".example"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	// The DNS has no all-numeric top-level domain and none spelled as a
	// hexadecimal literal, so a name shaped like one (".1", ".0xff") is a
	// non-canonical spelling of an address that resolves nowhere. Letters that
	// happen to be hexadecimal digits are ordinary domains (".de", ".cafe").
	label := host[strings.LastIndexByte(host, '.')+1:]
	permitted := "0123456789"
	if rest, found := strings.CutPrefix(label, "0x"); found {
		label, permitted = rest, "0123456789abcdef"
	}
	if label != "" && strings.IndexFunc(label, func(r rune) bool { return !strings.ContainsRune(permitted, r) }) < 0 {
		return false
	}
	// A single label is an intranet name; only a dotted domain is resolvable
	// from outside this network.
	return strings.Contains(host, ".")
}

// FuzzValidClientIDMetadataURL asserts the client identifier check never panics
// and never accepts a URL an authorization server could not dereference as a
// client_id.
func FuzzValidClientIDMetadataURL(f *testing.F) {
	for _, seed := range []string{
		"https://app.example.com/client-metadata.json",
		"https://app.example.com:8443/a/b.json",
		"http://app.example.com/c.json",
		"https://app.example.com/",
		"https://app.example.com/x.json?q=1",
		"https://app.example.com/x.json#f",
		"https://user@app.example.com/x.json",
		"https:///x.json",
		"https://127.0.0.1/x.json",
		"https://[::1]/x.json",
		"https://app.test/x.json",
		"https://\x00/x.json",
		"https://exämple.com/ünicode.json",
		"https://240.0.0.1/x.json",
		"https://100.64.0.1/x.json",
		"https://[2001:db8::1]/x.json",
		"https://app.test./x.json",
		"https://app.example.com./x.json",
		"https://intranet/x.json",
		"https://[fe80::1%25eth0]/x.json",
		"https://[2001:4860::1%25a.b]/x.json",
		"https://0x7f.1/x.json",
		"https://127.1/x.json",
		"https://0177.0.0.1/x.json",
		"https://app.example.123/x.json",
		"https://app.example.de/x.json",
		"https://app.example.com/a/../x.json",
		"https://app.example.com/x.json#",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		if !validClientIDMetadataURL(raw) {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted an unparsable url %q", raw)
		}
		if u.Scheme != "https" {
			t.Fatalf("accepted a non-https url %q", raw)
		}
		if u.Host == "" || u.Path == "" || u.Path == "/" {
			t.Fatalf("accepted %q without a host and path", raw)
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
			t.Fatalf("accepted %q with a query, fragment or user info", raw)
		}
		if strings.Contains(raw, "#") {
			t.Fatalf("accepted %q, which states a fragment", raw)
		}
		for _, segment := range strings.Split(u.Path, "/") {
			if segment == "." || segment == ".." {
				t.Fatalf("accepted %q, whose path carries a dot segment", raw)
			}
		}
		if !reachableFromTheInternet(t, u.Hostname()) {
			t.Fatalf("accepted %q, whose host no authorization server could fetch", raw)
		}
	})
}
