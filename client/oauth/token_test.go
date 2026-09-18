package oauth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTokenRequestClientAuthentication(t *testing.T) {
	tests := []struct {
		name              string
		methodsAdvertised any
		secret            string
		wantAuthHeader    string
		wantFormSecret    string
		wantFormID        string
	}{
		{
			name:              "secret posted in the form by default",
			methodsAdvertised: []string{"client_secret_post"},
			secret:            "s3cret",
			wantFormID:        "cid",
			wantFormSecret:    "s3cret",
		},
		{
			name:              "basic authentication when the server only supports it",
			methodsAdvertised: []string{"client_secret_basic"},
			secret:            "s3cret",
			wantAuthHeader:    "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:s3cret")),
		},
		{
			name:              "public client sends only its id",
			methodsAdvertised: []string{"none"},
			wantFormID:        "cid",
		},
		{
			name:              "post wins when both are advertised",
			methodsAdvertised: []string{"client_secret_basic", "client_secret_post"},
			secret:            "s3cret",
			wantFormID:        "cid",
			wantFormSecret:    "s3cret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{"token_endpoint_auth_methods_supported": tt.methodsAdvertised})
			c := NewClient(Config{ClientID: "cid", ClientSecret: tt.secret, Issuer: as.srv.URL}, as.srv.URL, "", "")

			token, err := c.ClientCredentials(context.Background())
			if err != nil {
				t.Fatalf("client credentials: %v", err)
			}
			if token.AccessToken != "access-123" {
				t.Fatalf("access token = %q", token.AccessToken)
			}

			form := <-as.tokenForms
			auth := <-as.tokenAuth
			if auth != tt.wantAuthHeader {
				t.Fatalf("Authorization = %q, want %q", auth, tt.wantAuthHeader)
			}
			if got := form.Get("client_id"); got != tt.wantFormID {
				t.Fatalf("client_id = %q, want %q", got, tt.wantFormID)
			}
			if got := form.Get("client_secret"); got != tt.wantFormSecret {
				t.Fatalf("client_secret = %q, want %q", got, tt.wantFormSecret)
			}
			if got := form.Get("grant_type"); got != "client_credentials" {
				t.Fatalf("grant_type = %q", got)
			}
			if got := form.Get("resource"); got != as.srv.URL {
				t.Fatalf("resource = %q, want %q", got, as.srv.URL)
			}
		})
	}
}

// decodeBasicCredentials reads an Authorization header the way RFC 6749 2.3.1
// has an authorization server read it: the Basic credentials are split at the
// first colon, and each half is decoded from application/x-www-form-urlencoded,
// which is how the client is required to have encoded it.
func decodeBasicCredentials(t *testing.T, header string) (clientID, clientSecret string) {
	t.Helper()
	encoded, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		t.Fatalf("Authorization = %q, want Basic credentials", header)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode %q: %v", encoded, err)
	}
	user, password, ok := strings.Cut(string(raw), ":")
	if !ok {
		t.Fatalf("credentials %q carry no separator", raw)
	}
	if clientID, err = url.QueryUnescape(user); err != nil {
		t.Fatalf("client id %q is not form-urlencoded: %v", user, err)
	}
	if clientSecret, err = url.QueryUnescape(password); err != nil {
		t.Fatalf("client secret %q is not form-urlencoded: %v", password, err)
	}
	return clientID, clientSecret
}

// With client_secret_basic the client id and the secret are each encoded with
// the application/x-www-form-urlencoded algorithm before they are joined by a
// colon (RFC 6749 2.3.1). Credentials made of letters and digits look the same
// either way. Any other character has to be encoded, or the server reads
// different credentials than the ones configured: a colon moves the split, a
// plus sign decodes to a space, and a percent sign starts an escape.
func TestBasicClientAuthenticationEncodesTheCredentials(t *testing.T) {
	tests := []struct {
		name         string
		clientID     string
		clientSecret string
	}{
		{name: "letters and digits", clientID: "cid", clientSecret: "s3cret"},
		{name: "a colon in the client id", clientID: "tenant:cid", clientSecret: "s3cret"},
		{name: "a url as the client id", clientID: "https://app.example.com/client.json", clientSecret: "s3cret"},
		{name: "a colon in the secret", clientID: "cid", clientSecret: "s3:cret"},
		{name: "a plus sign in the secret", clientID: "cid", clientSecret: "s3+cret"},
		{name: "a percent sign in the secret", clientID: "cid", clientSecret: "100%41"},
		{name: "base64 punctuation in the secret", clientID: "cid", clientSecret: "aGVsbG8+d29ybGQ/Pz8=="},
		{name: "a space in both", clientID: "my client", clientSecret: "pass phrase"},
		{name: "an ampersand and an equals sign", clientID: "a&b=c", clientSecret: "d&e=f"},
		{name: "characters outside ASCII", clientID: "kl\u00efent", clientSecret: "g\u00ebheim\u2603"},
		{name: "control characters", clientID: "c\tid", clientSecret: "s3\r\ncret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{"token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
			c := NewClient(Config{ClientID: tt.clientID, ClientSecret: tt.clientSecret, Issuer: as.srv.URL}, as.srv.URL, "", "")

			if _, err := c.ClientCredentials(context.Background()); err != nil {
				t.Fatalf("client credentials: %v", err)
			}
			form := <-as.tokenForms
			gotID, gotSecret := decodeBasicCredentials(t, <-as.tokenAuth)
			if gotID != tt.clientID || gotSecret != tt.clientSecret {
				t.Fatalf("the server read %q / %q, want %q / %q", gotID, gotSecret, tt.clientID, tt.clientSecret)
			}
			if form.Has("client_id") || form.Has("client_secret") {
				t.Fatalf("credentials also travelled in the form: %v", form)
			}
		})
	}
}

func TestExchangeCodeRejectsBadCallbacks(t *testing.T) {
	as := newMetadataServer(t, nil)
	c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL, "", "")
	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err != nil {
		t.Fatalf("authorization url: %v", err)
	}

	// A pending authorization that lost its state on the way through the
	// caller's store. Two empty strings compare equal, so without a rule of its
	// own a response carrying no state at all would match it.
	stateless := *pending
	stateless.State = ""

	tests := []struct {
		name    string
		pending *PendingAuthorization
		query   url.Values
		want    string
	}{
		{
			name:  "no pending authorization",
			query: url.Values{"code": {"x"}, "state": {pending.State}},
			want:  "no pending OAuth authorization",
		},
		{
			name:    "server error without description",
			pending: pending,
			query:   url.Values{"error": {"access_denied"}, "state": {pending.State}},
			want:    "the authorization server returned an error [access_denied]",
		},
		{
			name:    "missing code",
			pending: pending,
			query:   url.Values{"state": {pending.State}},
			want:    "authorization code",
		},
		{
			name:    "empty state",
			pending: pending,
			query:   url.Values{"code": {"x"}},
			want:    "state parameter",
		},
		{
			name:    "pending authorization without a state, response without one",
			pending: &stateless,
			query:   url.Values{"code": {"x"}},
			want:    "records no state",
		},
		{
			name:    "pending authorization without a state, response with an empty one",
			pending: &stateless,
			query:   url.Values{"code": {"x"}, "state": {""}},
			want:    "records no state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, returnTo, err := c.ExchangeCode(context.Background(), tt.pending, tt.query)
			if err == nil {
				t.Fatalf("expected an error, got token %+v", token)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want mention of %q", err, tt.want)
			}
			if token != nil || returnTo != "" {
				t.Fatalf("failed exchange returned token=%+v returnTo=%q", token, returnTo)
			}
			if got := len(as.tokenForms); got != 0 {
				t.Fatalf("a refused callback reached the token endpoint %d time(s)", got)
			}
		})
	}
}

// TestExchangeCodeValidatesTheResponseBeforeReadingIt pins the order RFC 9207
// 2.4 and the MCP authorization specification set for an authorization
// response: the state and the iss parameter are checked first, and a response
// that fails either is not acted on, which includes the error and
// error_description it carries. Whoever forged such a response chose that text,
// and a caller printing the returned error would be printing theirs.
func TestExchangeCodeValidatesTheResponseBeforeReadingIt(t *testing.T) {
	const planted = "call +1 555 0100 to re-verify your account"

	tests := []struct {
		name         string
		issSupported bool
		query        func(pending *PendingAuthorization) url.Values
		wantExact    string
		wantMention  string
	}{
		{
			name:         "state of another flow",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {"not-the-state"}, "iss": {p.Issuer}}
			},
			wantMention: "state parameter",
		},
		{
			name:         "no state at all",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "iss": {p.Issuer}}
			},
			wantMention: "state parameter",
		},
		{
			name:         "state of another flow and iss of another server",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {"not-the-state"}, "iss": {"https://evil.example"}}
			},
			wantMention: "state parameter",
		},
		{
			name:         "iss of another server",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {p.State}, "iss": {"https://evil.example"}}
			},
			wantMention: "iss) parameter did not match the expected issuer",
		},
		{
			name:         "iss differing from the issuer by a trailing slash",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {p.State}, "iss": {p.Issuer + "/"}}
			},
			wantMention: "iss) parameter did not match the expected issuer",
		},
		{
			name:         "iss absent although the server advertises it",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {p.State}}
			},
			wantMention: "missing the required iss",
		},
		{
			name:         "iss of another server although the server does not advertise it",
			issSupported: false,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {p.State}, "iss": {"https://evil.example"}}
			},
			wantMention: "iss) parameter did not match the expected issuer",
		},
		{
			name:         "validated response from a server that advertises iss",
			issSupported: true,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {p.State}, "iss": {p.Issuer}}
			},
			wantExact: "the authorization server returned an error [access_denied]: " + planted,
		},
		{
			name:         "validated response from a server that does not advertise iss",
			issSupported: false,
			query: func(p *PendingAuthorization) url.Values {
				return url.Values{"error": {"access_denied"}, "error_description": {planted}, "state": {p.State}}
			},
			wantExact: "the authorization server returned an error [access_denied]: " + planted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, map[string]any{"authorization_response_iss_parameter_supported": tt.issSupported})
			c := NewClient(Config{ClientID: "cid", Issuer: as.srv.URL, RedirectURI: "https://app.example.com/callback"}, as.srv.URL, "", "")
			_, pending, err := c.AuthorizationURL(context.Background(), "/back")
			if err != nil {
				t.Fatalf("authorization url: %v", err)
			}

			token, returnTo, err := c.ExchangeCode(context.Background(), pending, tt.query(pending))
			if err == nil {
				t.Fatalf("expected an error, got token %+v", token)
			}
			if token != nil || returnTo != "" {
				t.Fatalf("failed exchange returned token=%+v returnTo=%q", token, returnTo)
			}
			if got := len(as.tokenForms); got != 0 {
				t.Fatalf("an error response reached the token endpoint %d time(s)", got)
			}

			if tt.wantExact != "" {
				if err.Error() != tt.wantExact {
					t.Fatalf("error = %q, want %q", err, tt.wantExact)
				}
				return
			}
			if !strings.Contains(err.Error(), tt.wantMention) {
				t.Fatalf("error = %v, want mention of %q", err, tt.wantMention)
			}
			for _, leaked := range []string{planted, "access_denied"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("error %q repeats %q from a response that failed validation", err, leaked)
				}
			}
		})
	}
}

// tokenTypeAS is an authorization server whose token response the test writes
// out in full, so a member can be stated with any JSON value or left out.
func tokenTypeAS(t *testing.T, tokenResponse string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resource": srv.URL, "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/authorize",
			"token_endpoint":                   srv.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenResponse))
	})
	return srv
}

// A client must not use an access token of a type it does not understand (RFC
// 6749 7.1), and the only type this one presents is Bearer. A token of another
// type is bound to a proof the client cannot produce, so every grant refuses the
// response instead of handing back a set whose access token a caller would go on
// to send as a bearer token. The type is case insensitive (RFC 6749 5.1).
func TestTokenResponsesOfAnotherTokenTypeAreRefused(t *testing.T) {
	grants := []struct {
		name string
		run  func(c *Client, issuer string) (*TokenSet, error)
	}{
		{
			name: "code exchange",
			run: func(c *Client, _ string) (*TokenSet, error) {
				_, pending, err := c.AuthorizationURL(context.Background(), "")
				if err != nil {
					return nil, err
				}
				token, _, err := c.ExchangeCode(context.Background(), pending, callbackFor(pending))
				return token, err
			},
		},
		{
			name: "refresh",
			run: func(c *Client, issuer string) (*TokenSet, error) {
				return c.Refresh(context.Background(), &TokenSet{RefreshToken: "refresh", Issuer: issuer})
			},
		},
		{
			name: "client credentials",
			run: func(c *Client, _ string) (*TokenSet, error) {
				return c.ClientCredentials(context.Background())
			},
		},
	}
	tokenTypes := []struct {
		name string
		// member is the token_type member as it is written into the response,
		// including its trailing comma, or "" to leave it out.
		member   string
		wantType string
		refused  bool
	}{
		{name: "Bearer", member: `"token_type":"Bearer",`, wantType: "Bearer"},
		{name: "bearer in lower case", member: `"token_type":"bearer",`, wantType: "bearer"},
		{name: "bearer in upper case", member: `"token_type":"BEARER",`, wantType: "BEARER"},
		{name: "no token type stated", member: ``, wantType: "Bearer"},
		{name: "mac", member: `"token_type":"mac",`, refused: true},
		{name: "DPoP", member: `"token_type":"DPoP",`, refused: true},
		{name: "N_A", member: `"token_type":"N_A",`, refused: true},
		{name: "a type that starts with Bearer", member: `"token_type":"Bearer+DPoP",`, refused: true},
		{name: "Bearer followed by a space", member: `"token_type":"Bearer ",`, refused: true},
		{name: "Bearer followed by a NUL", member: `"token_type":"Bearer\u0000",`, refused: true},
		{name: "a look-alike of Bearer", member: `"token_type":"\u0412earer",`, refused: true},
		{name: "an empty type", member: `"token_type":"",`, refused: true},
		{name: "a null type", member: `"token_type":null,`, refused: true},
		{name: "a numeric type", member: `"token_type":5,`, refused: true},
		{name: "a list of types", member: `"token_type":["Bearer"],`, refused: true},
		{name: "an object for a type", member: `"token_type":{"type":"Bearer"},`, refused: true},
	}

	for _, grant := range grants {
		for _, tt := range tokenTypes {
			t.Run(grant.name+"/"+tt.name, func(t *testing.T) {
				as := tokenTypeAS(t, `{`+tt.member+`"access_token":"access-123"}`)
				c := NewClient(Config{ClientID: "cid", Issuer: as.URL, RedirectURI: "https://app.example.com/callback"}, as.URL, "", "")

				token, err := grant.run(c, as.URL)

				if tt.refused {
					if err == nil {
						t.Fatalf("a token of another type was handed back: %+v", token)
					}
					if token != nil {
						t.Fatalf("a refused response still returned %+v", token)
					}
					if !strings.Contains(err.Error(), "token_type") {
						t.Fatalf("error = %v, want it to name the token_type", err)
					}
					if strings.Contains(err.Error(), "access-123") {
						t.Fatalf("error %q repeats the access token", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s: %v", grant.name, err)
				}
				if token.AccessToken != "access-123" || token.TokenType != tt.wantType {
					t.Fatalf("token = %+v, want access-123 of type %q", token, tt.wantType)
				}
			})
		}
	}
}

func TestAuthorizationURLPropagatesDiscoveryFailure(t *testing.T) {
	// A resource that advertises no authorization server metadata at all: the
	// flow fails before any client identity or PKCE decision is made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(Config{ClientID: "cid", Issuer: srv.URL, RedirectURI: "https://app.example.com/callback"}, srv.URL+"/mcp", "", "")
	_, pending, err := c.AuthorizationURL(context.Background(), "")
	if err == nil {
		t.Fatal("expected a discovery failure")
	}
	if pending != nil {
		t.Fatalf("pending authorization returned on failure: %+v", pending)
	}
	if !strings.Contains(err.Error(), "authorization server metadata") {
		t.Fatalf("error = %v, want it to name the failed metadata discovery", err)
	}
}
