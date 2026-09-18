package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestParseChallenge(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   Challenge
	}{
		{"empty", "", Challenge{}},
		{
			"full",
			`Bearer resource_metadata="https://a/.well-known/x", error="invalid_token", error_description="expired", scope="mcp:use"`,
			Challenge{
				ResourceMetadataURL: "https://a/.well-known/x",
				Error:               "invalid_token",
				ErrorDescription:    "expired",
				Scope:               "mcp:use",
			},
		},
		{"unquoted", `Bearer error=invalid_request`, Challenge{Error: "invalid_request"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseChallenge(tc.header)
			if *got != tc.want {
				t.Fatalf("ParseChallenge(%q) = %+v, want %+v", tc.header, *got, tc.want)
			}
		})
	}
}

func TestGeneratePKCE(t *testing.T) {
	p, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if p.Verifier == "" || p.Challenge == "" {
		t.Fatal("empty pkce")
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Fatalf("challenge mismatch: %q want %q", p.Challenge, want)
	}
	// Two generations differ.
	p2, _ := GeneratePKCE()
	if p2.Verifier == p.Verifier {
		t.Fatal("verifier should be random per call")
	}
}

func TestTokenSetExpiry(t *testing.T) {
	now := time.Unix(1_000, 0)
	tok := tokenSetFromResponse(map[string]any{
		"access_token": "a",
		"expires_in":   float64(60),
	}, now)
	if tok.TokenType != "Bearer" {
		t.Fatalf("default token type = %q", tok.TokenType)
	}
	if tok.Expired(now) {
		t.Fatal("should not be expired at issue")
	}
	if !tok.Expired(now.Add(61 * time.Second)) {
		t.Fatal("should be expired after lifetime")
	}

	noExpiry := tokenSetFromResponse(map[string]any{"access_token": "a"}, now)
	if noExpiry.Expired(now.Add(100 * time.Hour)) {
		t.Fatal("token without expiry should never expire")
	}
}

func TestRequireSecure(t *testing.T) {
	tests := []struct {
		url    string
		wantOK bool
	}{
		{"https://example.com", true},
		{"http://localhost:8080/x", true},
		{"http://127.0.0.1/x", true},
		{"http://example.com", false},
		{"ftp://example.com", false},
	}
	for _, tc := range tests {
		err := requireSecure(tc.url)
		if (err == nil) != tc.wantOK {
			t.Fatalf("requireSecure(%q) err=%v, wantOK=%v", tc.url, err, tc.wantOK)
		}
	}
}

// requireNotInternal reads the URL and nothing else: it answers for a host
// written as a loopback name or as an internal address, ahead of any request,
// and it is the only check the authorization endpoint gets, since this client
// never connects to it. A name that merely resolves inward is refused where the
// connection is opened (TestDiscoveryNeverConnectsToAnInternalHost).
func TestRequireNotInternal(t *testing.T) {
	const public = "https://mcp.example.com/mcp"

	tests := []struct {
		name     string
		endpoint string
		resource string
		wantOK   bool
	}{
		{name: "a public host", endpoint: "https://as.example.com/token", resource: public, wantOK: true},
		{name: "localhost for a localhost resource", endpoint: "http://localhost/token", resource: "http://localhost/mcp", wantOK: true},
		{name: "the loopback address for a localhost resource", endpoint: "http://127.0.0.1:9000/token", resource: "http://localhost:8080/mcp", wantOK: true},
		{name: "the IPv6 loopback address for a loopback resource", endpoint: "http://[::1]:9000/token", resource: "http://127.0.0.1:8080/mcp", wantOK: true},
		{name: "localhost for a public resource", endpoint: "http://localhost/token", resource: public},
		{name: "localhost in upper case for a public resource", endpoint: "https://LOCALHOST/token", resource: public},
		{name: "the loopback address for a public resource", endpoint: "https://127.0.0.1/token", resource: public},
		{name: "the IPv6 loopback address for a public resource", endpoint: "https://[::1]/token", resource: public},
		{name: "the loopback address mapped into IPv6", endpoint: "https://[::ffff:127.0.0.1]/token", resource: public},
		{name: "another address of the loopback block", endpoint: "https://127.0.0.2/token", resource: public},
		{name: "a private address", endpoint: "https://10.0.0.1/token", resource: public},
		{name: "a private address for a localhost resource", endpoint: "https://10.0.0.1/token", resource: "http://localhost/mcp"},
		{name: "another private block", endpoint: "https://192.168.1.10/token", resource: public},
		{name: "the cloud metadata address", endpoint: "https://169.254.169.254/latest/meta-data", resource: public},
		{name: "a link-local IPv6 address", endpoint: "https://[fe80::1]/token", resource: public},
		{name: "a unique local IPv6 address", endpoint: "https://[fd00::1]/token", resource: public},
		{name: "the unspecified address", endpoint: "https://0.0.0.0/token", resource: public},
		{name: "an endpoint that does not parse", endpoint: "https://%zz/token", resource: public},
		{name: "a resource that does not parse", endpoint: "https://as.example.com/token", resource: "https://%zz/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireNotInternal(tt.endpoint, tt.resource)
			if (err == nil) != tt.wantOK {
				t.Fatalf("requireNotInternal(%q, %q) err=%v, wantOK=%v", tt.endpoint, tt.resource, err, tt.wantOK)
			}
		})
	}
}

func TestResolveTokenAuthMethod(t *testing.T) {
	none := resolveTokenAuthMethod(&AuthServerMetadata{}, "")
	if none != authMethodNone {
		t.Fatalf("no secret should yield none, got %q", none)
	}
	basicOnly := resolveTokenAuthMethod(&AuthServerMetadata{
		TokenEndpointAuthMethodsSupported: []string{authMethodBasic},
	}, "secret")
	if basicOnly != authMethodBasic {
		t.Fatalf("basic-only server should yield basic, got %q", basicOnly)
	}
	def := resolveTokenAuthMethod(&AuthServerMetadata{}, "secret")
	if def != authMethodPost {
		t.Fatalf("default should be post, got %q", def)
	}
}

func TestMetadataURLs(t *testing.T) {
	got, err := metadataURLs("https://issuer.example.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0] != "https://issuer.example.com/.well-known/oauth-authorization-server" {
		t.Fatalf("urls = %v", got)
	}

	got, _ = metadataURLs("https://issuer.example.com/tenant1")
	if len(got) != 3 || got[2] != "https://issuer.example.com/tenant1/.well-known/openid-configuration" {
		t.Fatalf("path-insertion urls = %v", got)
	}
}
