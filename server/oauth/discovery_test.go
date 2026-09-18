package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clientoauth "github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity/router"
)

// discoveryServer starts a listener serving one MCP route that always refuses,
// with the challenge middleware in front of it and this package's discovery
// documents mounted beside it. The origin is derived per request, so the
// documents describe the address the client actually reached. mcpPath is the
// route pattern for the MCP endpoint, spelled the way the client spells the
// request, or a wildcard where a pattern cannot express the path.
func discoveryServer(t *testing.T, mcpPath string) *httptest.Server {
	t.Helper()

	cfg := Config{
		AuthorizationEndpoint: "/oauth/authorize",
		TokenEndpoint:         "/oauth/token",
	}

	r := router.NewV2()
	r.Post(mcpPath, ok).Use(Challenge(cfg), rejectWritten)
	Routes(r, cfg)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// challengeFor makes the unauthenticated call a client starts with and returns
// the parsed challenge from the 401.
func challengeFor(t *testing.T, resourceURL string) *clientoauth.Challenge {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, resourceURL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call %s: %v", resourceURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	return clientoauth.ParseChallenge(resp.Header.Get(HeaderWWWAuthenticate))
}

// The discovery documents are only worth anything if a client can get from a
// 401 to the authorization server with them, so the whole handshake is driven
// here rather than each document read on its own: the challenge names a
// metadata URL, the document at that URL names a resource, and RFC 9728 3.3
// makes the client refuse to go on unless that resource is the one it was
// trying to reach. A path that ends in a slash is a different resource from one
// that does not (RFC 3986 6.2.3 only equates an empty path with "/"), so an
// endpoint mounted with one cannot be described with the other.
func TestDiscovery_RoundTripsThroughTheClient(t *testing.T) {
	tests := []struct {
		name     string
		mcpPath  string
		resource string
	}{
		{name: "a resource at a path", mcpPath: "/mcp", resource: "/mcp"},
		{name: "a resource whose path ends in a slash", mcpPath: "/mcp", resource: "/mcp/"},
		{name: "a nested resource", mcpPath: "/tenants/acme/mcp", resource: "/tenants/acme/mcp"},
		{name: "a resource at the root", mcpPath: "/", resource: "/"},
		// The other spelling of the root: an identifier with no path at all,
		// which RFC 3986 6.2.3 makes the same resource as the one above and
		// which a request line cannot be told apart from it.
		{name: "a resource identified without a path", mcpPath: "/", resource: ""},
		// An encoded octet is part of the identifier: a client that decoded it
		// and encoded it again could arrive at another resource, and for %2F at
		// another path depth entirely.
		{name: "a resource with an encoded space", mcpPath: "/tenants/acme%20corp/mcp", resource: "/tenants/acme%20corp/mcp"},
		{name: "a resource with an encoded separator", mcpPath: "/tenants/{rest:.*}", resource: "/tenants/a%2Fb/mcp"},
		{name: "a resource with an encoded non-ASCII segment", mcpPath: "/tenants/caf%C3%A9/mcp", resource: "/tenants/caf%C3%A9/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := discoveryServer(t, tt.mcpPath)
			resourceURL := srv.URL + tt.resource

			challenge := challengeFor(t, resourceURL)
			if challenge.ResourceMetadataURL == "" {
				t.Fatal("the challenge advertised no metadata URL")
			}

			// Both routes a client can take: the URL the challenge named, and
			// the one it derives itself when a server advertises none.
			for _, metadataURL := range []string{challenge.ResourceMetadataURL, ""} {
				named := "advertised"
				if metadataURL == "" {
					named = "derived"
				}
				t.Run(named, func(t *testing.T) {
					result, err := clientoauth.NewDiscovery().Discover(context.Background(), resourceURL, metadataURL)
					if err != nil {
						t.Fatalf("discovery for %s: %v", resourceURL, err)
					}
					if result.Server.Issuer != srv.URL {
						t.Errorf("issuer = %q, want %q", result.Server.Issuer, srv.URL)
					}
					if result.Server.AuthorizationEndpoint != srv.URL+"/oauth/authorize" {
						t.Errorf("authorization_endpoint = %q, want %q", result.Server.AuthorizationEndpoint, srv.URL+"/oauth/authorize")
					}
					if result.Server.TokenEndpoint != srv.URL+"/oauth/token" {
						t.Errorf("token_endpoint = %q, want %q", result.Server.TokenEndpoint, srv.URL+"/oauth/token")
					}
					if len(result.ScopesSupported) != 1 || result.ScopesSupported[0] != DefaultScope {
						t.Errorf("scopes_supported = %v, want [%s]", result.ScopesSupported, DefaultScope)
					}
				})
			}
		})
	}
}

// The other side of the same rule: a document that describes some other
// resource is refused, so the identifiers this package publishes have to be the
// ones clients ask about rather than merely be accepted by a lenient check.
func TestDiscovery_RejectsADocumentForAnotherResource(t *testing.T) {
	srv := discoveryServer(t, "/mcp")

	tests := []struct {
		name     string
		resource string
		metadata string
	}{
		{
			name:     "a slashed resource answered by the unslashed document",
			resource: "/mcp/",
			metadata: "/.well-known/oauth-protected-resource/mcp",
		},
		{
			name:     "an unslashed resource answered by the slashed document",
			resource: "/mcp",
			metadata: "/.well-known/oauth-protected-resource/mcp/",
		},
		{
			name:     "a resource answered by a sibling's document",
			resource: "/mcp",
			metadata: "/.well-known/oauth-protected-resource/other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := clientoauth.NewDiscovery().Discover(context.Background(), srv.URL+tt.resource, srv.URL+tt.metadata)
			if err == nil {
				t.Fatal("discovery accepted a document describing another resource")
			}
			if !strings.Contains(err.Error(), "did not match the expected resource") {
				t.Fatalf("error = %v, want the resource mismatch", err)
			}
		})
	}
}
