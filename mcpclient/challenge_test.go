package mcpclient

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// challengingResource is an MCP server that refuses an unauthenticated caller
// with the challenge the test scripts, and publishes its protected-resource
// metadata only at the paths the test scripts. It records every path it is asked
// for, so a test can assert where the flow went looking.
type challengingResource struct {
	srv *httptest.Server
	// challenge is the WWW-Authenticate value of the 401, or "" for a server
	// that serves no MCP endpoint at all.
	challenge string
	// published maps a path to the authorization server the document there
	// names.
	published map[string]string

	mu    sync.Mutex
	asked []string
}

func newChallengingResource(t *testing.T) *challengingResource {
	t.Helper()
	r := &challengingResource{published: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.EscapedPath()
		if path == "/mcp" {
			if r.challenge == "" {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("WWW-Authenticate", r.challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.mu.Lock()
		r.asked = append(r.asked, path)
		r.mu.Unlock()
		issuer, ok := r.published[path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		writeJSON(w, map[string]any{"resource": r.srv.URL + "/mcp", "authorization_servers": []string{issuer}})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// metadataRequests returns the metadata paths asked for so far, in order.
func (r *challengingResource) metadataRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// The MCP authorization specification has a client use the metadata URL named
// in the server's challenge when there is one, and derive the well-known
// locations only when there is not. The redirect route starts from a link in
// the application and not from a refused request, so it has to ask the server
// for that challenge itself: a server that publishes its metadata through the
// challenge alone is otherwise out of reach, and so is the scope it asks for.
func TestRedirectUsesTheMetadataTheChallengeAdvertises(t *testing.T) {
	const (
		insertedLocation = "/.well-known/oauth-protected-resource/mcp"
		rootLocation     = "/.well-known/oauth-protected-resource"
	)

	tests := []struct {
		name string
		// script sets what the resource answers; as is the authorization server
		// its documents name.
		script    func(r *challengingResource, as string)
		wantAsked []string
		wantScope string
	}{
		{
			name: "metadata published only where the challenge says",
			script: func(r *challengingResource, as string) {
				r.challenge = `Bearer resource_metadata="` + r.srv.URL + `/custom/metadata", scope="files:read"`
				r.published["/custom/metadata"] = as
			},
			wantAsked: []string{"/custom/metadata"},
			wantScope: "files:read",
		},
		{
			name: "a challenge that names a scope and no metadata",
			script: func(r *challengingResource, as string) {
				r.challenge = `Bearer scope="files:read files:write"`
				r.published[insertedLocation] = as
			},
			wantAsked: []string{insertedLocation},
			wantScope: "files:read files:write",
		},
		{
			name: "a challenge that names neither",
			script: func(r *challengingResource, as string) {
				r.challenge = `Bearer realm="mcp"`
				r.published[rootLocation] = as
			},
			wantAsked: []string{insertedLocation, rootLocation},
			wantScope: "configured",
		},
		{
			name: "a server that does not challenge",
			script: func(r *challengingResource, as string) {
				r.published[insertedLocation] = as
			},
			wantAsked: []string{insertedLocation},
			wantScope: "configured",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as := fakeAS(t)
			resource := newChallengingResource(t)
			tt.script(resource, as.URL)

			name := fmt.Sprintf("challenge-%d", i)
			RegisterClient(name, resource.srv.URL+"/mcp")
			p := OAuthRoutesFor(name, oauth.Config{ClientID: "cid", Issuer: as.URL, Scope: "configured"}, WithStore(&failingStore{}))
			call := mountModule(t, p)

			authorize := startFlowURL(t, call, "http://localhost:4000", "/mcp/oauth/"+name+"/redirect")

			if got := authorize.Scheme + "://" + authorize.Host; got != as.URL {
				t.Fatalf("the browser was sent to %q, want the declared authorization server %q", got, as.URL)
			}
			if got := resource.metadataRequests(); !reflect.DeepEqual(got, tt.wantAsked) {
				t.Fatalf("metadata was looked for at %v, want %v", got, tt.wantAsked)
			}
			if got := authorize.Query().Get("scope"); got != tt.wantScope {
				t.Fatalf("scope = %q, want %q", got, tt.wantScope)
			}
			if got := authorize.Query().Get("resource"); got != resource.srv.URL+"/mcp" {
				t.Fatalf("resource = %q, want %q", got, resource.srv.URL+"/mcp")
			}
		})
	}
}

// The metadata URL of a challenge is chosen by the server being connected to,
// which makes it the first URL a hostile server can aim at the network this
// application runs in. "localhost." is a name outside the loopback exemption
// that resolves inward, so only the connection guard can refuse it.
func TestRedirectDoesNotFollowAChallengeToAnInternalHost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		accepted int
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted++
			_ = conn.Close()
		}
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", listener.Addr(), err)
	}

	resource := newChallengingResource(t)
	resource.challenge = `Bearer resource_metadata="https://localhost.:` + port + `/metadata"`
	RegisterClient("challenge-internal", resource.srv.URL+"/mcp")
	p := OAuthRoutesFor("challenge-internal", oauth.Config{ClientID: "cid", Issuer: "https://as.example.com"}, WithStore(&failingStore{}))
	call := mountModule(t, p)

	rec := call(http.MethodGet, "http://localhost:4000/mcp/oauth/challenge-internal/redirect")

	_ = listener.Close()
	<-done
	if accepted != 0 {
		t.Fatalf("the flow opened %d connection(s) to the internal host the challenge named", accepted)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatalf("a refused flow must not redirect the browser: %q", rec.Header().Get("Location"))
	}
	if got := resource.metadataRequests(); len(got) != 0 {
		t.Fatalf("the flow fell back to %v although the challenge named where the metadata lives", got)
	}
}
