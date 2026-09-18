package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/auth"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/server"
)

// tokenScheme resolves one identity from one bearer credential, the smallest
// thing that behaves like the scheme an application guards an endpoint with.
type tokenScheme struct {
	token string
	user  auth.Authenticatable
}

func (s tokenScheme) Check(r *http.Request) bool { return s.User(r) != nil }

func (s tokenScheme) User(r *http.Request) auth.Authenticatable {
	if r != nil && r.Header.Get("Authorization") == "Bearer "+s.token {
		return s.user
	}
	return nil
}

func (s tokenScheme) ID(*http.Request) any                            { return nil }
func (s tokenScheme) SetUserStore(auth.UserStore)                     {}
func (s tokenScheme) Logout(http.ResponseWriter, *http.Request) error { return nil }

func (s tokenScheme) Login(http.ResponseWriter, *http.Request, auth.Authenticatable, ...bool) error {
	return nil
}

func (s tokenScheme) LoginByID(http.ResponseWriter, *http.Request, any, ...bool) error { return nil }

func (s tokenScheme) Attempt(http.ResponseWriter, *http.Request, map[string]any, ...bool) (bool, error) {
	return false, nil
}

// notVelocitysManager is a contract.AuthManager that is not velocity's concrete
// manager, the shape a replacement implementation or a test double takes.
type notVelocitysManager struct{}

func (notVelocitysManager) Allows(*http.Request, string, ...any) bool     { return false }
func (notVelocitysManager) Authorize(*http.Request, string, ...any) error { return nil }

// whoamiServer serves a tool that reports the identity resolved for the call,
// under the default scheme or a named one.
func whoamiServer(t *testing.T) *server.Server {
	t.Helper()
	report := func(scheme ...string) func(context.Context, *server.Request) (*server.Response, error) {
		return func(_ context.Context, req *server.Request) (*server.Response, error) {
			user := req.User(scheme...)
			if user == nil {
				return server.Text("anonymous"), nil
			}
			named, ok := user.(*auth.AuthUser)
			if !ok {
				return server.Text("unknown"), nil
			}
			return server.Text(named.Name), nil
		}
	}
	return server.New("transport-auth-test", "0.0.1",
		server.WithTools(
			server.NewTool("whoami", "report the caller").HandleFunc(report()),
			server.NewTool("whoami-api", "report the caller under the api scheme").HandleFunc(report("api")),
		),
	)
}

const whoamiBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`

// managerWithSchemes builds the two-scheme setup a real application has: a
// default one and a second one an API endpoint is guarded by.
func managerWithSchemes() *auth.Manager {
	m := auth.NewManager()
	m.RegisterScheme("web", tokenScheme{token: "session", user: &auth.AuthUser{ID: uint(7), Name: "Ada"}})
	m.RegisterScheme("api", tokenScheme{token: "api-key", user: &auth.AuthUser{ID: uint(9), Name: "Grace"}})
	m.SetDefaultScheme("web")
	return m
}

// callWhoami drives one tools/call through the transport handler on a route
// carrying services, and returns the text the handler reported.
func callWhoami(t *testing.T, services *velapp.Services, body, credential string) string {
	t.Helper()

	c, w := postContext(t, body)
	if services != nil {
		c.SetServices(services)
	}
	if credential != "" {
		c.Request.Header.Set("Authorization", "Bearer "+credential)
	}
	if err := Handler(whoamiServer(t))(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	resp := decodeResponse(t, w.Body.Bytes())
	if resp.Error != nil {
		t.Fatalf("tools/call returned error: %+v", resp.Error)
	}
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode result %s: %v", resp.Result, err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("result = %s, want one content block", w.Body.String())
	}
	return result.Content[0].Text
}

// The transport is what puts the caller's identity within reach of a handler, so
// every shape an application's auth wiring can take is answered here.
func TestHTTPIdentity(t *testing.T) {
	const apiCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami-api","arguments":{}}}`

	tests := []struct {
		name       string
		services   *velapp.Services
		body       string
		credential string
		want       string
	}{
		{
			name: "no service container at all",
			body: whoamiBody,
			want: "anonymous",
		},
		{
			name:     "an application with no auth configured",
			services: &velapp.Services{},
			body:     whoamiBody,
			want:     "anonymous",
		},
		{
			name:     "an auth manager that is not velocity's own",
			services: &velapp.Services{Auth: notVelocitysManager{}},
			body:     whoamiBody,
			want:     "anonymous",
		},
		{
			name:     "no credential on a configured application",
			services: &velapp.Services{Auth: managerWithSchemes()},
			body:     whoamiBody,
			want:     "anonymous",
		},
		{
			name:       "the default scheme's credential",
			services:   &velapp.Services{Auth: managerWithSchemes()},
			body:       whoamiBody,
			credential: "session",
			want:       "Ada",
		},
		{
			name:       "a named scheme's credential read under that scheme",
			services:   &velapp.Services{Auth: managerWithSchemes()},
			body:       apiCall,
			credential: "api-key",
			want:       "Grace",
		},
		{
			name:       "a named scheme's credential read under the default scheme",
			services:   &velapp.Services{Auth: managerWithSchemes()},
			body:       whoamiBody,
			credential: "api-key",
			want:       "anonymous",
		},
		{
			name:       "the default scheme's credential read under a named scheme",
			services:   &velapp.Services{Auth: managerWithSchemes()},
			body:       apiCall,
			credential: "session",
			want:       "anonymous",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := callWhoami(t, tt.services, tt.body, tt.credential); got != tt.want {
				t.Fatalf("handler saw %q, want %q", got, tt.want)
			}
		})
	}
}

// A scheme the application never registered resolves nobody rather than falling
// back to the default one, which would hand a handler the wrong identity.
func TestHTTPIdentity_UnknownSchemeResolvesNobody(t *testing.T) {
	manager := managerWithSchemes()
	c, _ := postContext(t, whoamiBody)
	c.SetServices(&velapp.Services{Auth: manager})
	c.Request.Header.Set("Authorization", "Bearer session")

	resolve := identityResolver(c)
	if resolve == nil {
		t.Fatal("no resolver for an authenticated request")
	}
	if got := resolve("nope"); got != nil {
		t.Fatalf("resolve(%q) = %#v, want nil", "nope", got)
	}
	if got := resolve(""); got == nil {
		t.Fatal("resolve(\"\") returned nobody for an authenticated request")
	}
}

// The identity must be read off the request that arrived, not off the router
// context, which velocity resets and hands to the next request the moment the
// handler returns. A resolver that outlives its handler has to keep answering
// for its own request.
func TestHTTPIdentity_SurvivesTheRouterContext(t *testing.T) {
	manager := managerWithSchemes()
	c, _ := postContext(t, whoamiBody)
	c.SetServices(&velapp.Services{Auth: manager})
	c.Request.Header.Set("Authorization", "Bearer session")

	resolve := identityResolver(c)
	if resolve == nil {
		t.Fatal("no resolver for an authenticated request")
	}

	// What the router does on the way out: the context is emptied and reused.
	c.Response = nil
	c.Request = nil

	got := resolve("")
	if got == nil {
		t.Fatal("the resolver stopped answering once its context was recycled")
	}
	if named, ok := got.(*auth.AuthUser); !ok || named.Name != "Ada" {
		t.Fatalf("resolver answered %#v, want Ada", got)
	}
}

// A request with nothing behind it (a context built by hand, a transport that
// never had one) yields no resolver rather than a panic.
func TestHTTPIdentity_NoRequestYieldsNoResolver(t *testing.T) {
	if got := identityResolver(nil); got != nil {
		t.Fatal("a nil context produced a resolver")
	}
	if got := identityResolver(&router.Context{}); got != nil {
		t.Fatal("a context with no request produced a resolver")
	}
}

// A streamed reply is served by a different branch of the handler than the
// buffered one, and it carries its own request context. A caller authenticated
// for a tools/call that reports progress must reach the handler there too,
// otherwise identity would silently vanish the moment a client asked for an
// event stream.
func TestHTTPIdentity_StreamedPath(t *testing.T) {
	whoamiProgress := server.NewTool("whoami-progress", "report the caller while reporting progress").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			_ = req.ReportProgress(server.ProgressUpdate{Progress: 1, Total: 1})
			user := req.User()
			if user == nil {
				return server.Text("anonymous"), nil
			}
			named, ok := user.(*auth.AuthUser)
			if !ok {
				return server.Text("unknown"), nil
			}
			return server.Text(named.Name), nil
		})
	srv := server.New("transport-auth-stream-test", "0.0.1", server.WithTools(whoamiProgress))

	// The progressToken plus an event-stream-only Accept is what puts the call
	// on the streamed path; the tool then commits the SSE framing by reporting
	// progress before it resolves the caller.
	const body = `{"jsonrpc":"2.0","id":3,"method":"tools/call",` +
		`"params":{"name":"whoami-progress","arguments":{},"_meta":{"progressToken":"tok"}}}`

	tests := []struct {
		name       string
		credential string
		want       string
	}{
		{name: "no credential", want: "anonymous"},
		{name: "the default scheme's credential", credential: "session", want: "Ada"},
		{name: "another scheme's credential", credential: "api-key", want: "anonymous"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, w := postContext(t, body, "Accept", "text/event-stream")
			c.SetServices(&velapp.Services{Auth: managerWithSchemes()})
			if tt.credential != "" {
				c.Request.Header.Set("Authorization", "Bearer "+tt.credential)
			}
			if err := Handler(srv)(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", ct)
			}

			// Two frames: the progress notification that committed the stream,
			// then the result naming whoever the call authenticated.
			frames := sseFrames(w.Body.String())
			if len(frames) != 2 {
				t.Fatalf("want 2 SSE frames (progress + result), got %d: %q", len(frames), frames)
			}
			resp := decodeResponse(t, []byte(frames[1]))
			if resp.Error != nil {
				t.Fatalf("streamed tools/call returned error: %+v", resp.Error)
			}
			var result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				t.Fatalf("decode result %s: %v", resp.Result, err)
			}
			if result.IsError || len(result.Content) != 1 {
				t.Fatalf("result = %s, want one content block", frames[1])
			}
			if got := result.Content[0].Text; got != tt.want {
				t.Fatalf("streamed handler saw %q, want %q", got, tt.want)
			}
		})
	}
}

// One handler serves every client at once, and each call must see its own
// caller: a resolver shared or captured across requests would cross them.
func TestHTTPIdentity_Concurrent(t *testing.T) {
	services := &velapp.Services{Auth: managerWithSchemes()}
	srv := whoamiServer(t)
	h := Handler(srv)

	credentials := []string{"session", "api-key", ""}
	want := map[string]string{"session": "Ada", "api-key": "anonymous", "": "anonymous"}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		credential := credentials[i%len(credentials)]
		wg.Add(1)
		go func() {
			defer wg.Done()

			c, w := router.NewTestContext(http.MethodPost, "/mcp", strings.NewReader(whoamiBody))
			c.SetServices(services)
			if credential != "" {
				c.Request.Header.Set("Authorization", "Bearer "+credential)
			}
			if err := h(c); err != nil {
				t.Errorf("handler returned error: %v", err)
				return
			}
			if got := w.Body.String(); !strings.Contains(got, `"`+want[credential]+`"`) {
				t.Errorf("credential %q saw %s, want %q", credential, got, want[credential])
			}
		}()
	}
	wg.Wait()
}
