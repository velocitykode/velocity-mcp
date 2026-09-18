package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resourceServer serves the two discovery documents a client reads, dispatching
// on the raw request path rather than through a mux so nothing normalizes the
// path before the assertions see it. It records the protected-resource path the
// client asked for and answers with the resource identifier the test names.
type resourceServer struct {
	srv *httptest.Server
	// declared is the identifier the protected-resource document claims. Empty
	// means the document echoes the URL the client built its request from.
	declared string
	asked    chan string
}

func newResourceServer(t *testing.T) *resourceServer {
	t.Helper()

	rs := &resourceServer{asked: make(chan string, 8)}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.HasPrefix(path, "/.well-known/oauth-authorization-server"):
			writeJSON(w, map[string]any{
				"issuer":                           rs.srv.URL,
				"authorization_endpoint":           rs.srv.URL + "/authorize",
				"token_endpoint":                   rs.srv.URL + "/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case strings.HasPrefix(path, "/.well-known/oauth-protected-resource"):
			rs.asked <- path
			resource := rs.declared
			if resource == "" {
				resource = rs.srv.URL + strings.TrimPrefix(path, "/.well-known/oauth-protected-resource")
			}
			writeJSON(w, map[string]any{
				"resource":              resource,
				"authorization_servers": []string{rs.srv.URL},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

// RFC 9728 3.1 builds the metadata URL by inserting the well-known path between
// the host and the path of the resource identifier, so the path has to survive
// the trip exactly: a resource whose path ends in a slash is a different
// resource from one whose path does not, and an encoded segment names a
// different segment once it is decoded. Nothing else of the identifier is part
// of that URL.
func TestDiscoveryDerivesTheMetadataURLFromTheResourcePath(t *testing.T) {
	tests := []struct {
		name string
		// resource is appended to the server's base URL to form the identifier
		// discovery is asked about.
		resource string
		// declared is what the document claims, appended the same way. Empty
		// means the document echoes the path it was asked at.
		declared string
		want     string
	}{
		{name: "a path", resource: "/mcp", want: "/.well-known/oauth-protected-resource/mcp"},
		{name: "a path ending in a slash", resource: "/mcp/", want: "/.well-known/oauth-protected-resource/mcp/"},
		{name: "a nested path", resource: "/tenants/acme/mcp", want: "/.well-known/oauth-protected-resource/tenants/acme/mcp"},
		{name: "an encoded segment", resource: "/tenants/a%2Fb/mcp", want: "/.well-known/oauth-protected-resource/tenants/a%2Fb/mcp"},
		// A bare slash and no path at all are the same identifier (RFC 3986
		// 6.2.3), and both address the unsuffixed document.
		{name: "a bare slash", resource: "/", want: "/.well-known/oauth-protected-resource"},
		{name: "no path at all", resource: "", want: "/.well-known/oauth-protected-resource"},
		// RFC 9728 3.1 names the host and the path of the identifier, so a
		// query is carried by the identifier but not by the metadata URL.
		{
			name:     "a query is no part of the metadata URL",
			resource: "/mcp?tenant=acme",
			declared: "/mcp?tenant=acme",
			want:     "/.well-known/oauth-protected-resource/mcp",
		},
		{
			// RFC 9728 2 forbids a fragment in a resource identifier, so a
			// client that was handed one discovers the resource without it.
			name:     "a fragment is no part of the identifier",
			resource: "/mcp#section",
			declared: "/mcp",
			want:     "/.well-known/oauth-protected-resource/mcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newResourceServer(t)
			if tt.declared != "" {
				rs.declared = rs.srv.URL + tt.declared
			}

			if _, err := NewDiscovery().Discover(context.Background(), rs.srv.URL+tt.resource, ""); err != nil {
				t.Fatalf("discover %s: %v", rs.srv.URL+tt.resource, err)
			}

			select {
			case got := <-rs.asked:
				if got != tt.want {
					t.Fatalf("fetched metadata at %q, want %q", got, tt.want)
				}
			default:
				t.Fatal("no protected-resource metadata was fetched")
			}
		})
	}
}

// RFC 9728 3.3 has the client refuse a document whose resource is not the one
// it asked about. The only spelling difference that is not a different resource
// is the empty path against a bare slash, which RFC 3986 6.2.3 defines as the
// same URI and which a request line cannot tell apart in any case. That
// equivalence is about the path alone: a slash that ends a query value is part
// of that value, and two identifiers that differ there are two resources.
func TestDiscoveryAcceptsOnlyTheEmptyPathEquivalenceOfTheResource(t *testing.T) {
	tests := []struct {
		name string
		// resource and declared are appended to the server's base URL, unless
		// declaredAbsolute names the document's claim outright.
		resource         string
		declared         string
		declaredAbsolute string
		wantErr          bool
	}{
		{name: "the root spelled with a slash, described without one", resource: "/", declared: ""},
		{name: "the root spelled without a slash, described with one", resource: "", declared: "/"},
		{
			name:     "the root with a query, described with the slash the equivalence covers",
			resource: "?tenant=acme",
			declared: "/?tenant=acme",
		},
		{
			name:     "the root with a query, described without that slash",
			resource: "/?tenant=acme",
			declared: "?tenant=acme",
		},
		{
			name:     "a query value that ends in a slash is not the empty path",
			resource: "/?tenant=",
			declared: "/?tenant=/",
			wantErr:  true,
		},
		{
			name:     "a query that differs",
			resource: "/?tenant=acme",
			declared: "/?tenant=other",
			wantErr:  true,
		},
		{
			// The identifier carries no fragment (RFC 9728 2), so a document
			// that appends one describes something the client did not ask for.
			name:     "a document that appends a fragment",
			resource: "/mcp",
			declared: "/mcp#section",
			wantErr:  true,
		},
		{
			name:     "a document that appends a fragment to the root",
			resource: "/",
			declared: "/#/",
			wantErr:  true,
		},
		{
			// A fragment stated as empty is still a fragment, and it parses to
			// none: a comparison made on the parsed value would not see it.
			name:     "a document that appends an empty fragment to the root",
			resource: "/",
			declared: "/#",
			wantErr:  true,
		},
		{
			name:     "a document that appends an empty fragment to the root spelled without a slash",
			resource: "",
			declared: "/#",
			wantErr:  true,
		},
		{
			name:     "a document that appends an empty fragment to a path",
			resource: "/mcp",
			declared: "/mcp#",
			wantErr:  true,
		},
		{name: "a path described with a trailing slash it did not have", resource: "/mcp", declared: "/mcp/", wantErr: true},
		{name: "a path described without the trailing slash it had", resource: "/mcp/", declared: "/mcp", wantErr: true},
		{name: "a path described as a sibling", resource: "/mcp", declared: "/other", wantErr: true},
		{
			name:             "a document describing another host altogether",
			resource:         "/mcp",
			declaredAbsolute: "https://attacker.example/mcp",
			wantErr:          true,
		},
		{
			name:             "a document whose identifier is not a URL",
			resource:         "/mcp",
			declaredAbsolute: "https://%zz/mcp",
			wantErr:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newResourceServer(t)
			rs.declared = rs.srv.URL + tt.declared
			if tt.declaredAbsolute != "" {
				rs.declared = tt.declaredAbsolute
			}

			_, err := NewDiscovery().Discover(context.Background(), rs.srv.URL+tt.resource, "")
			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("discovery accepted a document describing %q", rs.declared)
			case tt.wantErr && !strings.Contains(err.Error(), "did not match the expected resource"):
				t.Fatalf("error = %v, want the resource mismatch", err)
			case !tt.wantErr && err != nil:
				t.Fatalf("discovery refused an equivalent identifier: %v", err)
			}
		})
	}
}

// A resource identifier that names no authority gives the client nowhere to
// look for the metadata document (RFC 9728 3.1), so discovery stops before it
// asks anything of the network.
func TestDiscoveryRejectsAResourceItCannotAddress(t *testing.T) {
	tests := []struct {
		name     string
		resource string
	}{
		{name: "a relative reference", resource: "/mcp"},
		{name: "an absolute URL with no authority", resource: "https:///mcp"},
		{name: "an unparseable URL", resource: "://broken"},
		{name: "nothing at all", resource: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewDiscovery().Discover(context.Background(), tt.resource, "")
			if err == nil {
				t.Fatalf("discovery accepted the resource %q", tt.resource)
			}
			if !strings.Contains(err.Error(), "unable to parse URL") {
				t.Fatalf("error = %v, want the parse failure", err)
			}
		})
	}
}
