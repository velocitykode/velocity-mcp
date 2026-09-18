package oauth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// internalService listens on the loopback interface the way a service inside
// the application's network would, and counts the connections that reach it. It
// speaks no protocol: what the tests ask is whether a connection was opened at
// all, which is the whole of a server-side request forgery.
type internalService struct {
	listener net.Listener
	port     string
	accepted atomic.Int64
	done     chan struct{}
}

func newInternalService(t *testing.T) *internalService {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", listener.Addr(), err)
	}
	s := &internalService{listener: listener, port: port, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Counted before the connection is closed: a client that got this far
			// only gives up once the close reaches it, so the count is settled
			// by the time the call under test returns.
			s.accepted.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { s.connections() })
	return s
}

// connections stops the service and reports how many connections reached it.
func (s *internalService) connections() int64 {
	_ = s.listener.Close()
	<-s.done
	return s.accepted.Load()
}

// url returns an https URL on this service spelled with the given host.
func (s *internalService) url(host, path string) string {
	return "https://" + host + ":" + s.port + path
}

// internalHostSpellings are ways of writing a host that reaches the loopback
// interface. The names are answered from the hosts file on every platform, so
// resolving them touches no network; the literals need no resolving at all.
//
// Only what the name resolves to tells the first group from a public host, which
// is why reading the URL cannot: "localhost." is a fully qualified domain name
// like any other until it is looked up.
var internalHostSpellings = []struct {
	name string
	host string
}{
	{name: "the loopback name as an absolute domain name", host: "localhost."},
	{name: "that name in upper case", host: "LOCALHOST."},
	{name: "that name in mixed case", host: "LocalHost."},
	{name: "the loopback name", host: "localhost"},
	{name: "the loopback address", host: "127.0.0.1"},
	{name: "the loopback address mapped into IPv6", host: "[::ffff:127.0.0.1]"},
	{name: "the mapped address in hexadecimal groups", host: "[::ffff:7f00:1]"},
}

// A protected resource chooses the metadata URL it advertises in its challenge,
// so that URL is the first thing a hostile resource can aim at the network the
// client runs in. No spelling of an internal host may get a connection opened.
func TestDiscoveryNeverConnectsToAnInternalHost(t *testing.T) {
	for _, tt := range internalHostSpellings {
		t.Run(tt.name, func(t *testing.T) {
			internal := newInternalService(t)
			advertised := internal.url(tt.host, "/.well-known/oauth-protected-resource/mcp")

			result, err := NewDiscovery().Discover(context.Background(), "https://mcp.example.com/mcp", advertised)
			if err == nil {
				t.Fatalf("discovery used %s: %+v", advertised, result)
			}
			if got := internal.connections(); got != 0 {
				t.Fatalf("discovery opened %d connection(s) to %s (error: %v)", got, advertised, err)
			}
		})
	}
}

// loopbackResource is a protected resource on the loopback interface, the
// development arrangement, whose metadata the test controls. A resource there
// may name an authorization server on the loopback interface too, and nothing
// else inside the network.
type loopbackResource struct {
	srv      *httptest.Server
	requests atomic.Int64
	// server holds the authorization-server metadata members the test
	// overrides, keyed by member name.
	server map[string]any
	// issuer overrides the authorization server the resource names.
	issuer string
}

func newLoopbackResource(t *testing.T) *loopbackResource {
	t.Helper()
	r := &loopbackResource{server: map[string]any{}}
	mux := http.NewServeMux()
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		r.requests.Add(1)
		issuer := r.srv.URL
		if r.issuer != "" {
			issuer = r.issuer
		}
		writeJSON(w, map[string]any{"resource": r.srv.URL + "/mcp", "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		r.requests.Add(1)
		doc := map[string]any{
			"issuer":                           r.srv.URL,
			"authorization_endpoint":           r.srv.URL + "/authorize",
			"token_endpoint":                   r.srv.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		}
		for k, v := range r.server {
			doc[k] = v
		}
		writeJSON(w, doc)
	})
	return r
}

// Everything past the first document is advertised too: the authorization
// server the resource names, and the token and registration endpoints that
// server names. Each is a URL this client connects to, so each is refused an
// internal host as the connection is opened, including by a resource that is
// itself on the loopback interface: what it is exempted for is the loopback
// names as they are written, not whatever resolves there.
func TestAdvertisedEndpointsNeverReachAnInternalHost(t *testing.T) {
	const hostile = "localhost."

	// publicClient is a pre-registered public client of the authorization server
	// the loopback resource names by default, and unregistered a client that has
	// to register itself.
	publicClient := func(issuer string) Config {
		return Config{ClientID: "cid", Issuer: issuer, RedirectURI: "http://localhost/callback"}
	}
	unregistered := func(string) Config {
		return Config{RedirectURI: "http://localhost/callback"}
	}

	tests := []struct {
		name   string
		config func(issuer string) Config
		// aim points one advertised URL at the internal service.
		aim func(r *loopbackResource, internal *internalService)
		// run drives the flow that would connect to it.
		run func(c *Client, r *loopbackResource) error
	}{
		{
			name:   "the authorization server named by the resource",
			config: publicClient,
			aim: func(r *loopbackResource, internal *internalService) {
				r.issuer = internal.url(hostile, "")
			},
			run: func(c *Client, _ *loopbackResource) error {
				_, err := c.ClientCredentials(context.Background())
				return err
			},
		},
		{
			name:   "the token endpoint, reached by the client credentials grant",
			config: publicClient,
			aim: func(r *loopbackResource, internal *internalService) {
				r.server["token_endpoint"] = internal.url(hostile, "/token")
			},
			run: func(c *Client, _ *loopbackResource) error {
				_, err := c.ClientCredentials(context.Background())
				return err
			},
		},
		{
			name:   "the token endpoint, reached by a refresh",
			config: publicClient,
			aim: func(r *loopbackResource, internal *internalService) {
				r.server["token_endpoint"] = internal.url(hostile, "/token")
			},
			run: func(c *Client, r *loopbackResource) error {
				_, err := c.Refresh(context.Background(), &TokenSet{RefreshToken: "refresh", Issuer: r.srv.URL})
				return err
			},
		},
		{
			name:   "the token endpoint, reached by the code exchange",
			config: publicClient,
			aim: func(r *loopbackResource, internal *internalService) {
				r.server["token_endpoint"] = internal.url(hostile, "/token")
			},
			run: func(c *Client, _ *loopbackResource) error {
				_, pending, err := c.AuthorizationURL(context.Background(), "")
				if err != nil {
					return err
				}
				_, _, err = c.ExchangeCode(context.Background(), pending, callbackFor(pending))
				return err
			},
		},
		{
			name:   "the registration endpoint",
			config: unregistered,
			aim: func(r *loopbackResource, internal *internalService) {
				r.server["registration_endpoint"] = internal.url(hostile, "/register")
			},
			run: func(c *Client, _ *loopbackResource) error {
				_, _, err := c.AuthorizationURL(context.Background(), "")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			internal := newInternalService(t)
			resource := newLoopbackResource(t)
			tt.aim(resource, internal)

			c := NewClient(tt.config(resource.srv.URL), resource.srv.URL+"/mcp", "", "")
			err := tt.run(c, resource)
			if err == nil {
				t.Fatal("the flow succeeded against an endpoint on an internal host")
			}
			if got := internal.connections(); got != 0 {
				t.Fatalf("the flow opened %d connection(s) to the internal service (error: %v)", got, err)
			}
			// The loopback resource itself was reachable, so the refusal is the
			// advertised host and not a client that connects to nothing.
			if resource.requests.Load() == 0 {
				t.Fatal("the loopback resource was never asked for its metadata")
			}
		})
	}
}

// Config.AllowPrivateHosts is the consumer vouching for endpoints on a private
// network, and it has to reach the connection: a flag that relaxed the URL
// checks and left the dial-time guard in place would refuse the very hosts it
// was set for. The resource here is spelled "localhost.", a name outside the
// loopback exemption that resolves inward, so only the opt-in lets it through.
func TestAllowPrivateHostsReachesAnInternalHost(t *testing.T) {
	tests := []struct {
		name         string
		allowPrivate bool
		wantReached  bool
	}{
		{name: "refused by default", allowPrivate: false, wantReached: false},
		{name: "reached once the consumer opts in", allowPrivate: true, wantReached: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int64
			var base string
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			base = strings.Replace(srv.URL, "127.0.0.1", "localhost.", 1)

			mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeJSON(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeJSON(w, map[string]any{
					"issuer":                           base,
					"authorization_endpoint":           base + "/authorize",
					"token_endpoint":                   base + "/token",
					"code_challenge_methods_supported": []string{"S256"},
				})
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeJSON(w, map[string]any{"access_token": "access-123", "token_type": "Bearer"})
			})

			c := NewClient(Config{ClientID: "cid", Issuer: base, AllowPrivateHosts: tt.allowPrivate}, base+"/mcp", "", "")
			token, err := c.ClientCredentials(context.Background())

			if !tt.wantReached {
				if err == nil {
					t.Fatalf("the default posture issued a token from %s: %+v", base, token)
				}
				if got := requests.Load(); got != 0 {
					t.Fatalf("the default posture sent %d request(s) to %s", got, base)
				}
				return
			}
			if err != nil {
				t.Fatalf("client credentials against %s: %v", base, err)
			}
			if token.AccessToken != "access-123" {
				t.Fatalf("access token = %q", token.AccessToken)
			}
			// Discovery and the token request are separate clients; both have to
			// carry the opt-in for all three requests to arrive.
			if got := requests.Load(); got != 3 {
				t.Fatalf("requests = %d, want the two metadata documents and the token request", got)
			}
		})
	}
}

// The resource URL is the one address of a flow the application supplies, and
// the only thing that may widen what the flow connects to. It widens it for the
// loopback names as they are written, never for a name or a number that merely
// ends up there.
func TestPostureFollowsTheConfiguredResource(t *testing.T) {
	tests := []struct {
		name         string
		resource     string
		allowPrivate bool
		want         hostPosture
	}{
		{name: "a public resource", resource: "https://mcp.example.com/mcp", want: publicHosts},
		{name: "localhost", resource: "http://localhost:8080/mcp", want: loopbackHosts},
		{name: "localhost in upper case", resource: "http://LOCALHOST:8080/mcp", want: loopbackHosts},
		{name: "the IPv4 loopback address", resource: "http://127.0.0.1:8080/mcp", want: loopbackHosts},
		{name: "the IPv6 loopback address", resource: "http://[::1]:8080/mcp", want: loopbackHosts},
		{name: "localhost as an absolute name", resource: "http://localhost.:8080/mcp", want: publicHosts},
		{name: "a subdomain of localhost", resource: "http://app.localhost:8080/mcp", want: publicHosts},
		{name: "a name that starts with localhost", resource: "https://localhost.example.com/mcp", want: publicHosts},
		{name: "another address of the loopback block", resource: "http://127.0.0.2:8080/mcp", want: publicHosts},
		{name: "a short spelling of the loopback address", resource: "http://127.1:8080/mcp", want: publicHosts},
		{name: "the loopback address as one number", resource: "http://2130706433:8080/mcp", want: publicHosts},
		{name: "a private address", resource: "http://10.0.0.5/mcp", want: publicHosts},
		{name: "localhost as user info of another host", resource: "https://localhost@mcp.example.com/mcp", want: publicHosts},
		{name: "an unparseable resource", resource: "://broken", want: publicHosts},
		{name: "no resource", resource: "", want: publicHosts},
		{name: "a public resource with the opt-in", resource: "https://mcp.example.com/mcp", allowPrivate: true, want: privateHosts},
		{name: "a loopback resource with the opt-in", resource: "http://localhost:8080/mcp", allowPrivate: true, want: privateHosts},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postureFor(tt.allowPrivate, tt.resource); got != tt.want {
				t.Fatalf("postureFor(%v, %q) = %d, want %d", tt.allowPrivate, tt.resource, got, tt.want)
			}
		})
	}
}
