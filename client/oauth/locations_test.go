package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The two well-known locations of the protected-resource metadata for a
// resource at /mcp, and where the same server would publish
// authorization-server metadata if it were its own authorization server.
const (
	insertedLocation = "/.well-known/oauth-protected-resource/mcp"
	rootLocation     = "/.well-known/oauth-protected-resource"
	serverLocation   = "/.well-known/oauth-authorization-server"
)

// scriptedResource is a protected resource whose answer at each path the test
// scripts. It records every path it is asked for, in order, so a test can assert
// what discovery put on the wire and not only what it concluded. A path with no
// scripted answer is a 404.
type scriptedResource struct {
	srv     *httptest.Server
	answers map[string]http.HandlerFunc

	mu    sync.Mutex
	asked []string
}

func newScriptedResource(t *testing.T) *scriptedResource {
	t.Helper()
	r := &scriptedResource{answers: map[string]http.HandlerFunc{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.EscapedPath()
		r.mu.Lock()
		r.asked = append(r.asked, path)
		answer := r.answers[path]
		r.mu.Unlock()
		if answer == nil {
			http.NotFound(w, req)
			return
		}
		answer(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// requests returns the paths asked for so far, in order.
func (r *scriptedResource) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// document answers a path with a JSON object.
func document(members map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, members) }
}

// status answers a path with a bare status code.
func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// page answers a path the way a single-page application's catch-all route does:
// a 200 that carries markup instead of the document that was asked for.
func page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte("<!doctype html><title>app</title>"))
}

// hangUp drops the connection without answering.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// serverMetadata is the authorization-server metadata of issuer.
func serverMetadata(issuer string) map[string]any {
	return map[string]any{
		"issuer":                           issuer,
		"authorization_endpoint":           issuer + "/authorize",
		"token_endpoint":                   issuer + "/token",
		"code_challenge_methods_supported": []string{"S256"},
	}
}

// The MCP authorization specification lets a server publish its
// protected-resource metadata at the well-known URL built from the path of its
// MCP endpoint or at the one at the root, and has a client that was given no URL
// in a challenge ask them in that order. A server that publishes at the root
// only is as conformant as one that publishes at the path, and the authorization
// server it declares there is the one the flow has to use.
func TestDiscoveryFallsBackToTheRootWellKnownLocation(t *testing.T) {
	tests := []struct {
		name string
		// script sets the resource's answers. as is the authorization server
		// the published document names.
		script func(r *scriptedResource, as string)
		// wantIssuer is "as" for the declared authorization server and "origin"
		// for the resource's own origin; empty means discovery must fail.
		wantIssuer string
		wantErr    string
		wantAsked  []string
	}{
		{
			name: "published at the root only, describing the resource",
			script: func(r *scriptedResource, as string) {
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL + "/mcp", "authorization_servers": []string{as}})
			},
			wantIssuer: "as",
			wantAsked:  []string{insertedLocation, rootLocation},
		},
		{
			name: "published at the root only, describing the origin it was derived from",
			script: func(r *scriptedResource, as string) {
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL, "authorization_servers": []string{as}})
			},
			wantIssuer: "as",
			wantAsked:  []string{insertedLocation, rootLocation},
		},
		{
			name: "published at the root only, describing the origin with the slash of an empty path",
			script: func(r *scriptedResource, as string) {
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL + "/", "authorization_servers": []string{as}})
			},
			wantIssuer: "as",
			wantAsked:  []string{insertedLocation, rootLocation},
		},
		{
			name: "published at both, where the path location decides",
			script: func(r *scriptedResource, as string) {
				r.answers[insertedLocation] = document(map[string]any{"resource": r.srv.URL + "/mcp", "authorization_servers": []string{as}})
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL, "authorization_servers": []string{"https://other.example.com"}})
			},
			wantIssuer: "as",
			wantAsked:  []string{insertedLocation},
		},
		{
			name: "the path location answers with a page instead of a document",
			script: func(r *scriptedResource, as string) {
				r.answers[insertedLocation] = page
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL + "/mcp", "authorization_servers": []string{as}})
			},
			wantIssuer: "as",
			wantAsked:  []string{insertedLocation, rootLocation},
		},
		{
			name: "the path location is refused to an unauthenticated caller",
			script: func(r *scriptedResource, as string) {
				r.answers[insertedLocation] = status(http.StatusUnauthorized)
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL + "/mcp", "authorization_servers": []string{as}})
			},
			wantIssuer: "as",
			wantAsked:  []string{insertedLocation, rootLocation},
		},
		{
			name: "the root describes a sibling of the resource",
			script: func(r *scriptedResource, as string) {
				r.answers[rootLocation] = document(map[string]any{"resource": r.srv.URL + "/other", "authorization_servers": []string{as}})
			},
			wantErr:   "did not match the expected resource",
			wantAsked: []string{insertedLocation, rootLocation},
		},
		{
			name: "the root describes a resource on another host",
			script: func(r *scriptedResource, as string) {
				r.answers[rootLocation] = document(map[string]any{"resource": "https://someone-else.example.com", "authorization_servers": []string{as}})
			},
			wantErr:   "did not match the expected resource",
			wantAsked: []string{insertedLocation, rootLocation},
		},
		{
			name: "the path location describes the origin, which only the root may",
			script: func(r *scriptedResource, as string) {
				r.answers[insertedLocation] = document(map[string]any{"resource": r.srv.URL, "authorization_servers": []string{as}})
			},
			wantErr:   "did not match the expected resource",
			wantAsked: []string{insertedLocation},
		},
		{
			// A server that publishes authorization-server metadata and no
			// protected-resource metadata at all: every location answered, and
			// each answered that it holds nothing.
			name: "published nowhere",
			script: func(r *scriptedResource, _ string) {
				r.answers[serverLocation] = document(serverMetadata(r.srv.URL))
			},
			wantIssuer: "origin",
			wantAsked:  []string{insertedLocation, rootLocation, serverLocation},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := newMetadataServer(t, nil)
			resource := newScriptedResource(t)
			tt.script(resource, as.srv.URL)

			result, err := NewDiscovery().Discover(context.Background(), resource.srv.URL+"/mcp", "")

			if got := resource.requests(); !reflect.DeepEqual(got, tt.wantAsked) {
				t.Fatalf("the resource was asked for %v, want %v (error: %v)", got, tt.wantAsked, err)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want mention of %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			want := as.srv.URL
			if tt.wantIssuer == "origin" {
				want = resource.srv.URL
			}
			if result.Server.Issuer != want {
				t.Fatalf("issuer = %q, want %q", result.Server.Issuer, want)
			}
		})
	}
}

// A location that could not answer has said nothing about what the resource
// publishes. Discovery stops there: carrying on as though the resource declared
// no authorization server would let an outage, or anyone able to cause one, move
// the flow from the server the resource names to the resource's own origin.
func TestDiscoveryDoesNotGuessWhenALocationCannotAnswer(t *testing.T) {
	tests := []struct {
		name      string
		script    func(r *scriptedResource)
		wantErr   string
		wantAsked []string
	}{
		{
			name:      "the path location fails with a server error",
			script:    func(r *scriptedResource) { r.answers[insertedLocation] = status(http.StatusInternalServerError) },
			wantErr:   "failed with status [500]",
			wantAsked: []string{insertedLocation},
		},
		{
			name:      "the path location is unavailable",
			script:    func(r *scriptedResource) { r.answers[insertedLocation] = status(http.StatusServiceUnavailable) },
			wantErr:   "failed with status [503]",
			wantAsked: []string{insertedLocation},
		},
		{
			name:      "the path location is rate limited",
			script:    func(r *scriptedResource) { r.answers[insertedLocation] = status(http.StatusTooManyRequests) },
			wantErr:   "failed with status [429]",
			wantAsked: []string{insertedLocation},
		},
		{
			name:      "the path location times the request out",
			script:    func(r *scriptedResource) { r.answers[insertedLocation] = status(http.StatusRequestTimeout) },
			wantErr:   "failed with status [408]",
			wantAsked: []string{insertedLocation},
		},
		{
			name:      "the path location drops the connection",
			script:    func(r *scriptedResource) { r.answers[insertedLocation] = hangUp },
			wantErr:   "request to [",
			wantAsked: []string{insertedLocation},
		},
		{
			name:      "the root location fails after the path location had nothing",
			script:    func(r *scriptedResource) { r.answers[rootLocation] = status(http.StatusBadGateway) },
			wantErr:   "failed with status [502]",
			wantAsked: []string{insertedLocation, rootLocation},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := newScriptedResource(t)
			// The resource also answers as an authorization server, so a
			// discovery that guessed would succeed against its origin.
			resource.answers[serverLocation] = document(serverMetadata(resource.srv.URL))
			tt.script(resource)

			result, err := NewDiscovery().Discover(context.Background(), resource.srv.URL+"/mcp", "")
			if err == nil {
				t.Fatalf("discovery settled on %q without an answer from the resource", result.Server.Issuer)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want mention of %q", err, tt.wantErr)
			}
			if got := resource.requests(); !reflect.DeepEqual(got, tt.wantAsked) {
				t.Fatalf("the resource was asked for %v, want %v", got, tt.wantAsked)
			}
		})
	}
}

// RFC 9728 2 makes resource a required member and 3.3 has the client refuse a
// document whose resource is not the one it asked about. A document that names
// none cannot pass that check, wherever it was found: it would hand the client
// an authorization server with nothing tying it to the resource.
func TestDiscoveryRefusesMetadataThatDeclaresNoResource(t *testing.T) {
	declarations := []struct {
		name     string
		resource any
		declared bool
	}{
		{name: "no resource member"},
		{name: "an empty resource", resource: "", declared: true},
		{name: "a null resource", resource: nil, declared: true},
		{name: "a numeric resource", resource: 42, declared: true},
		{name: "a list of resources", resource: []string{"https://mcp.example.com/mcp"}, declared: true},
	}
	locations := []struct {
		name string
		path string
		// advertised reports whether the location is handed to discovery as
		// the URL a challenge named, instead of being derived.
		advertised bool
	}{
		{name: "at the advertised location", path: "/metadata", advertised: true},
		{name: "at the path location", path: insertedLocation},
		{name: "at the root location", path: rootLocation},
	}

	for _, location := range locations {
		for _, declaration := range declarations {
			t.Run(location.name+"/"+declaration.name, func(t *testing.T) {
				as := newMetadataServer(t, nil)
				resource := newScriptedResource(t)
				members := map[string]any{"authorization_servers": []string{as.srv.URL}}
				if declaration.declared {
					members["resource"] = declaration.resource
				}
				resource.answers[location.path] = document(members)

				advertised := ""
				if location.advertised {
					advertised = resource.srv.URL + location.path
				}
				result, err := NewDiscovery().Discover(context.Background(), resource.srv.URL+"/mcp", advertised)
				if err == nil {
					t.Fatalf("discovery followed a document naming no resource to %q", result.Server.Issuer)
				}
				if !strings.Contains(err.Error(), "does not declare the resource it describes") {
					t.Fatalf("error = %v, want the missing resource refusal", err)
				}
				if got := as.discoveries.Load(); got != 0 {
					t.Fatalf("the authorization server the document named was contacted %d time(s)", got)
				}
			})
		}
	}
}

// A URL named in a challenge is the only location there is: the resource said
// where its metadata lives, so an answer there that is no document is a failure
// and not a reason to go looking elsewhere.
func TestDiscoveryDoesNotLookPastTheAdvertisedLocation(t *testing.T) {
	tests := []struct {
		name    string
		answer  http.HandlerFunc
		wantErr string
	}{
		{name: "not found", answer: nil, wantErr: "yielded no metadata document (status [404])"},
		{name: "a page instead of a document", answer: page, wantErr: "yielded no metadata document (status [200])"},
		{name: "a server error", answer: status(http.StatusInternalServerError), wantErr: "failed with status [500]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := newScriptedResource(t)
			resource.answers[serverLocation] = document(serverMetadata(resource.srv.URL))
			resource.answers[rootLocation] = document(map[string]any{"resource": resource.srv.URL + "/mcp", "authorization_servers": []string{resource.srv.URL}})
			if tt.answer != nil {
				resource.answers["/metadata"] = tt.answer
			}

			_, err := NewDiscovery().Discover(context.Background(), resource.srv.URL+"/mcp", resource.srv.URL+"/metadata")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want mention of %q", err, tt.wantErr)
			}
			if got, want := resource.requests(), []string{"/metadata"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("the resource was asked for %v, want %v", got, want)
			}
		})
	}
}

// FuzzRequireResourceMatches feeds the resource member of a metadata document,
// which is whatever the server chose to write, to the check that decides whether
// the document is used. Whatever it accepts has to name the host the client is
// talking to: the property is stated on the parsed value, independently of how
// the check compares, and a panic on any input fails the target by itself.
func FuzzRequireResourceMatches(f *testing.F) {
	const (
		resource = "https://mcp.example.com/mcp"
		root     = "https://mcp.example.com"
	)
	for _, seed := range []string{
		resource,
		root,
		root + "/",
		"",
		"/",
		"//",
		"https:",
		"https://",
		"https:///mcp",
		"HTTPS://MCP.EXAMPLE.COM/mcp",
		"https://mcp.example.com/mcp/",
		"https://mcp.example.com/mcp?",
		"https://mcp.example.com/mcp#",
		"https://mcp.example.com/mcp#/",
		"https://mcp.example.com/?",
		"https://mcp.example.com/#",
		"https://mcp.example.com:443/mcp",
		"https://mcp.example.com./mcp",
		"https://mcp.example.com.attacker.example/mcp",
		"https://attacker.example/mcp",
		"https://attacker.example/https://mcp.example.com/mcp",
		"https://mcp.example.com@attacker.example/mcp",
		"https://attacker.example#@mcp.example.com/mcp",
		"https://attacker.example\\@mcp.example.com/mcp",
		"https://mcp.example.com%2Fmcp",
		"https://mcp.example.com/%6dcp",
		"https://mcp.example.com/mcp/../mcp",
		"https://%zz/mcp",
		"https://mcp.example.com/mcp\x00",
		"https://mcp.example.com/mcp\r\nresource: https://attacker.example",
		"http://mcp.example.com/mcp",
		"ftp://mcp.example.com/",
		" https://mcp.example.com/mcp",
		"https://mcp.example.com/mcp ",
		"https://mcp.exаmple.com/mcp",
		strings.Repeat("https://mcp.example.com/mcp", 512),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, declared string) {
		err := requireResourceMatches(map[string]any{"resource": declared}, resource, root)
		if err != nil {
			return
		}
		u, parseErr := url.Parse(declared)
		if parseErr != nil {
			t.Fatalf("accepted %q, which is not a URL: %v", declared, parseErr)
		}
		if u.Scheme != "https" || u.Host != "mcp.example.com" || u.User != nil {
			t.Fatalf("accepted %q, which names another origin", declared)
		}
		if u.Path != "" && u.Path != "/" && u.Path != "/mcp" {
			t.Fatalf("accepted %q, which names another path", declared)
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(declared, "#") {
			t.Fatalf("accepted %q, which carries a query or a fragment", declared)
		}
	})
}
