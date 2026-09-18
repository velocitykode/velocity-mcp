package module

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/auth"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity-mcp/server/oauth"
)

// bearerScheme resolves an identity from a bearer credential, the smallest
// thing that behaves like the scheme an application installs in front of its
// MCP endpoint.
type bearerScheme struct {
	token string
	user  auth.Authenticatable
}

func (s bearerScheme) Check(r *http.Request) bool { return s.User(r) != nil }

func (s bearerScheme) User(r *http.Request) auth.Authenticatable {
	if r != nil && r.Header.Get("Authorization") == "Bearer "+s.token {
		return s.user
	}
	return nil
}

func (s bearerScheme) ID(*http.Request) any                            { return nil }
func (s bearerScheme) SetUserStore(auth.UserStore)                     {}
func (s bearerScheme) Logout(http.ResponseWriter, *http.Request) error { return nil }

func (s bearerScheme) Login(http.ResponseWriter, *http.Request, auth.Authenticatable, ...bool) error {
	return nil
}

func (s bearerScheme) LoginByID(http.ResponseWriter, *http.Request, any, ...bool) error { return nil }

func (s bearerScheme) Attempt(http.ResponseWriter, *http.Request, map[string]any, ...bool) (bool, error) {
	return false, nil
}

// requireAuth is the guard an application attaches to its MCP route: it refuses
// anything the auth manager cannot resolve an identity for.
func requireAuth(next router.HandlerFunc) router.HandlerFunc {
	return func(c *router.Context) error {
		manager := auth.FromContext(c)
		if manager == nil || manager.User(c.Request) == nil {
			return router.NewHTTPError(http.StatusUnauthorized)
		}
		return next(c)
	}
}

// whoami names the identity a handler was given, in the one shape every
// primitive under test reports it: a plain string a wire assertion can read.
func whoami(req *server.Request, scheme ...string) string {
	user := req.User(scheme...)
	if user == nil {
		return "anonymous"
	}
	if named, ok := user.(*auth.AuthUser); ok {
		return named.Name
	}
	return "unknown"
}

// whoamiPrompt reports the caller from a prompts/get handler.
type whoamiPrompt struct{}

func (whoamiPrompt) Name() string        { return "whoami-prompt" }
func (whoamiPrompt) Description() string { return "report the caller" }

func (whoamiPrompt) Arguments() []server.PromptArgument { return nil }

func (whoamiPrompt) Handle(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text(whoami(req)), nil
}

// whoamiResource reports the caller from a resources/read handler.
type whoamiResource struct{}

func (whoamiResource) Name() string        { return "whoami-resource" }
func (whoamiResource) Description() string { return "report the caller" }
func (whoamiResource) URI() string         { return "mcp://whoami" }
func (whoamiResource) MimeType() string    { return "text/plain" }

func (whoamiResource) Read(ctx context.Context, req *server.Request) (*server.Response, error) {
	return server.Text(whoami(req)), nil
}

// whoamiServer serves one of each primitive, all reporting the identity the SDK
// resolved for the call, so the wire tests can see what a handler actually
// receives on every path that reaches one.
func whoamiServer() *server.Server {
	return server.New("oauth-module-test", "0.0.1",
		server.WithTools(
			server.NewTool("whoami", "report the caller").
				HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
					return server.Text(whoami(req)), nil
				}),
			// The streamed path reports progress before the result, which is
			// what makes the transport take it.
			server.NewTool("whoami-streamed", "report the caller while streaming").
				HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
					if err := req.ReportProgress(server.ProgressUpdate{Progress: 1, Total: 1}); err != nil {
						return nil, err
					}
					return server.Text(whoami(req)), nil
				}),
			// The scheme an MCP endpoint is guarded by is rarely the one the
			// application defaults to, so a handler must be able to name it.
			server.NewTool("whoami-api", "report the caller under the api scheme").
				HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
					return server.Text(whoami(req, "api")), nil
				}),
		),
		server.WithPrompts(whoamiPrompt{}),
		server.WithResources(whoamiResource{}),
	)
}

// oauthConfig is the protected-resource posture the tests mount.
func oauthConfig() oauth.Config {
	return oauth.Config{
		BaseURL:               "https://mcp.example.test",
		AuthorizationEndpoint: "/oauth/authorize",
		TokenEndpoint:         "/oauth/token",
	}
}

// mountProtected builds a router serving the module with OAuth wired, guarded by
// the bearer scheme, with the auth manager on the service container.
//
// Two schemes are registered, as a real application has: the default one a
// browser session would use, and a second one an API endpoint is guarded by.
// They accept different credentials and resolve different people, so a handler
// reading the wrong one is visible rather than coincidentally right.
func mountProtected(t *testing.T, opts ...Option) *router.VelocityRouterV2 {
	t.Helper()

	manager := auth.NewManager()
	manager.RegisterScheme("web", bearerScheme{token: "good", user: &auth.AuthUser{ID: uint(7), Name: "Ada"}})
	manager.RegisterScheme("api", bearerScheme{token: "api-key", user: &auth.AuthUser{ID: uint(9), Name: "Grace"}})
	manager.SetDefaultScheme("web")

	services := &velapp.Services{Auth: manager}
	r := router.NewV2()
	r.SetServices(services)

	opts = append([]Option{WithOAuth(oauthConfig()), WithMiddleware(requireAuth)}, opts...)
	New(whoamiServer(), opts...).Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))
	return r
}

// call drives one JSON-RPC message through the mounted MCP endpoint.
func call(r *router.VelocityRouterV2, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, DefaultPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

const whoamiCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`

// An unauthenticated call is refused with a challenge that tells the client
// exactly where to discover the authorization server.
func TestModuleOAuth_UnauthenticatedCallIsChallenged(t *testing.T) {
	rec := call(mountProtected(t), whoamiCall)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	const want = `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`
	if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != want {
		t.Fatalf("%s = %q, want %q", oauth.HeaderWWWAuthenticate, got, want)
	}
}

// Without WithOAuth there is no discovery to point a client at, so the refusal
// names the realm and the scope and nothing else. It still says that much: a
// bare 401 with no WWW-Authenticate leaves a client with nothing at all. The
// call carries no credentials, so no error code is stated (RFC 6750 3); the
// same call with a token that is refused is told the token is the problem.
func TestModuleOAuth_ChallengeWithoutDiscovery(t *testing.T) {
	services := &velapp.Services{}
	r := router.NewV2()
	r.SetServices(services)
	New(whoamiServer(), WithMiddleware(requireAuth)).Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	for _, tt := range []struct {
		name    string
		headers []string
		want    string
	}{
		{name: "no credentials", want: `Bearer realm="mcp", scope="mcp:use"`},
		{name: "a refused token", headers: []string{"Authorization", "Bearer expired-token"}, want: `Bearer realm="mcp", error="invalid_token", scope="mcp:use"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := call(r, whoamiCall, tt.headers...)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != tt.want {
				t.Fatalf("%s = %q, want %q", oauth.HeaderWWWAuthenticate, got, tt.want)
			}
		})
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("discovery status = %d, want 404 without WithOAuth", w.Code)
	}
}

// An application whose own guard already advertises where to authorize keeps
// that challenge: the module never mounts a route that strips the
// resource_metadata URL, scope or error a guard chose, with or without
// WithOAuth.
func TestModuleOAuth_ApplicationChallengeSurvives(t *testing.T) {
	const guardValue = `Bearer realm="other", resource_metadata="https://auth.example.test/.well-known/oauth-protected-resource", scope="admin", error="insufficient_scope"`

	denyWithChallenge := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			c.SetHeader(oauth.HeaderWWWAuthenticate, guardValue)
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Unauthenticated."})
		}
	}

	tests := []struct {
		name string
		opts []Option
	}{
		{name: "without WithOAuth", opts: []Option{WithMiddleware(denyWithChallenge)}},
		{name: "with WithOAuth", opts: []Option{WithOAuth(oauthConfig()), WithMiddleware(denyWithChallenge)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			services := &velapp.Services{}
			r := router.NewV2()
			r.SetServices(services)
			New(whoamiServer(), tt.opts...).Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

			rec := call(r, whoamiCall)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != guardValue {
				t.Fatalf("%s = %q, want the application's own challenge %q", oauth.HeaderWWWAuthenticate, got, guardValue)
			}
		})
	}
}

// An application that declares it publishes no protected-resource metadata gets
// the same posture through the module: no document mounted, and a challenge that
// does not claim one exists. The option and the routes always agree.
func TestModuleOAuth_WithoutResourceMetadataIsCoherent(t *testing.T) {
	cfg := oauthConfig()
	cfg.WithoutResourceMetadata = true

	services := &velapp.Services{}
	r := router.NewV2()
	r.SetServices(services)
	New(whoamiServer(), WithOAuth(cfg), WithMiddleware(requireAuth)).
		Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	rec := call(r, whoamiCall)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	const want = `Bearer realm="mcp", scope="mcp:use"`
	if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != want {
		t.Fatalf("%s = %q, want %q", oauth.HeaderWWWAuthenticate, got, want)
	}

	for _, target := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404 when the challenge names no document", target, w.Code)
		}
	}
}

// The posture the standalone consumer needs: the metadata document lives on the
// authorization server's own origin, this application publishes none of its own,
// and the challenge names the operator's URL exactly.
func TestModuleOAuth_PinnedMetadataURLOnAnotherOrigin(t *testing.T) {
	const metadataURL = "https://id.example.test/.well-known/oauth-protected-resource"

	services := &velapp.Services{}
	r := router.NewV2()
	r.SetServices(services)
	New(whoamiServer(),
		WithOAuth(oauth.Config{
			ResourceMetadataURL:     metadataURL,
			Scope:                   "provisioning",
			WithoutResourceMetadata: true,
		}),
		WithMiddleware(requireAuth),
	).Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	rec := call(r, whoamiCall)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	want := `Bearer realm="mcp", resource_metadata="` + metadataURL + `", scope="provisioning"`
	if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != want {
		t.Fatalf("%s = %q, want %q", oauth.HeaderWWWAuthenticate, got, want)
	}

	// Nothing of this application's own is published under the well-known path,
	// so no second document can contradict the one the challenge names.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("discovery status = %d, want 404", w.Code)
	}
}

// The identity the guard accepted is the identity the tool handler sees: the
// full path from the HTTP request through the transport into Request.User.
func TestModuleOAuth_AuthenticatedCallReachesTheHandlerWithTheUser(t *testing.T) {
	rec := call(mountProtected(t), whoamiCall, "Authorization", "Bearer good")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != "" {
		t.Fatalf("%s = %q, want none on a successful call", oauth.HeaderWWWAuthenticate, got)
	}

	var envelope struct {
		Error  *struct{ Message string } `json:"error"`
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
	}
	if envelope.Error != nil {
		t.Fatalf("tools/call errored: %s", envelope.Error.Message)
	}
	if envelope.Result == nil || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("result = %s, want one content block", rec.Body.String())
	}
	if got := envelope.Result.Content[0].Text; got != "Ada" {
		t.Fatalf("handler saw %q, want the authenticated user %q", got, "Ada")
	}
}

// Every primitive that takes a request is handed the same identity, over both
// the buffered and the streamed reply path. Each case names the JSON-RPC method
// it drives and reads the answer back off the wire.
func TestModuleOAuth_EveryPrimitiveSeesTheUser(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers []string
		want    string
	}{
		{
			name: "tools/call",
			body: whoamiCall,
			want: "Ada",
		},
		{
			name: "prompts/get",
			body: `{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"whoami-prompt","arguments":{}}}`,
			want: "Ada",
		},
		{
			name: "resources/read",
			body: `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"mcp://whoami"}}`,
			want: "Ada",
		},
		{
			// The streamed path builds its own context, so it has to carry the
			// identity the buffered one does.
			name:    "tools/call over the event stream",
			body:    `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami-streamed","arguments":{},"_meta":{"progressToken":"tok-1"}}}`,
			headers: []string{"Accept", "text/event-stream"},
			want:    "Ada",
		},
		{
			// A handler naming the scheme its endpoint is guarded by gets that
			// scheme's identity, not the application's default one.
			name: "a handler naming a non-default scheme",
			body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami-api","arguments":{}}}`,
			want: "anonymous",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := append([]string{"Authorization", "Bearer good"}, tt.headers...)
			rec := call(mountProtected(t), tt.body, headers...)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if got := reportedIdentity(t, rec.Body.String()); got != tt.want {
				t.Fatalf("handler saw %q, want %q", got, tt.want)
			}
		})
	}
}

// The credential the named scheme accepts resolves that scheme's identity, and
// only that one: the default scheme sees nobody for it.
func TestModuleOAuth_NamedSchemeResolvesItsOwnIdentity(t *testing.T) {
	// The route's guard accepts either credential, so the api credential gets
	// through to the handlers.
	acceptEither := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			manager := auth.FromContext(c)
			if manager == nil {
				return router.NewHTTPError(http.StatusUnauthorized)
			}
			for _, name := range []string{"web", "api"} {
				scheme, err := manager.Scheme(name)
				if err == nil && scheme.User(c.Request) != nil {
					return next(c)
				}
			}
			return router.NewHTTPError(http.StatusUnauthorized)
		}
	}

	manager := auth.NewManager()
	manager.RegisterScheme("web", bearerScheme{token: "good", user: &auth.AuthUser{ID: uint(7), Name: "Ada"}})
	manager.RegisterScheme("api", bearerScheme{token: "api-key", user: &auth.AuthUser{ID: uint(9), Name: "Grace"}})
	manager.SetDefaultScheme("web")

	services := &velapp.Services{Auth: manager}
	r := router.NewV2()
	r.SetServices(services)
	New(whoamiServer(), WithOAuth(oauthConfig()), WithMiddleware(acceptEither)).
		Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	tests := []struct {
		name, credential, body, want string
	}{
		{
			name:       "the api credential under the api scheme",
			credential: "api-key",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami-api","arguments":{}}}`,
			want:       "Grace",
		},
		{
			name:       "the api credential under the default scheme",
			credential: "api-key",
			body:       whoamiCall,
			want:       "anonymous",
		},
		{
			name:       "the session credential under the default scheme",
			credential: "good",
			body:       whoamiCall,
			want:       "Ada",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := call(r, tt.body, "Authorization", "Bearer "+tt.credential)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if got := reportedIdentity(t, rec.Body.String()); got != tt.want {
				t.Fatalf("handler saw %q, want %q", got, tt.want)
			}
		})
	}
}

// reportedIdentity reads the single text block out of a JSON-RPC reply, whether
// it arrived as a plain body or as SSE frames, and whatever primitive produced
// it. It fails the test rather than returning an empty string, so a reply that
// went wrong cannot pass as "anonymous".
func reportedIdentity(t *testing.T, body string) string {
	t.Helper()

	// The streamed reply is a run of "data: <json>" frames; the result is in
	// the last one, the progress notifications in the ones before it.
	if strings.HasPrefix(body, "data: ") {
		frames := []string{}
		for _, line := range strings.Split(body, "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				frames = append(frames, payload)
			}
		}
		if len(frames) < 2 {
			t.Fatalf("streamed reply carried %d frames, want a progress frame and a result: %q", len(frames), body)
		}
		body = frames[len(frames)-1]
	}

	var envelope struct {
		Error  *struct{ Message string } `json:"error"`
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Contents []struct {
				Text string `json:"text"`
			} `json:"contents"`
			Messages []struct {
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("call errored: %s", envelope.Error.Message)
	}
	if envelope.Result == nil || envelope.Result.IsError {
		t.Fatalf("result = %s, want a successful reply", body)
	}

	switch {
	case len(envelope.Result.Content) == 1:
		return envelope.Result.Content[0].Text
	case len(envelope.Result.Contents) == 1:
		return envelope.Result.Contents[0].Text
	case len(envelope.Result.Messages) == 1:
		return envelope.Result.Messages[0].Content.Text
	}
	t.Fatalf("result = %s, want exactly one text block", body)
	return ""
}

// A server reached over a transport with no HTTP request behind it still runs:
// Request.User answers nil rather than failing the call.
func TestModuleOAuth_HandlerWithoutAnAuthenticatedCall(t *testing.T) {
	services := &velapp.Services{}
	r := router.NewV2()
	r.SetServices(services)
	New(whoamiServer(), WithOAuth(oauthConfig())).Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	rec := call(r, whoamiCall)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "anonymous") {
		t.Fatalf("body = %s, want the handler to see no identity", rec.Body.String())
	}
}

// WithOAuth mounts the discovery documents next to the MCP route, so the URL
// the challenge names actually resolves.
func TestModuleOAuth_DiscoveryIsMountedAlongsideTheEndpoint(t *testing.T) {
	r := mountProtected(t)

	tests := []struct {
		target string
		want   map[string]any
	}{
		{
			target: "/.well-known/oauth-protected-resource/mcp",
			want: map[string]any{
				"resource":              "https://mcp.example.test/mcp",
				"authorization_servers": []any{"https://mcp.example.test"},
			},
		},
		{
			target: "/.well-known/oauth-authorization-server",
			want: map[string]any{
				"issuer":                 "https://mcp.example.test",
				"authorization_endpoint": "https://mcp.example.test/oauth/authorize",
				"token_endpoint":         "https://mcp.example.test/oauth/token",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.target, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			var doc map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode %s: %v", w.Body.String(), err)
			}
			for key, want := range tt.want {
				list, isList := want.([]any)
				if !isList {
					if doc[key] != want {
						t.Errorf("%s = %#v, want %#v", key, doc[key], want)
					}
					continue
				}
				got, ok := doc[key].([]any)
				if !ok || len(got) != len(list) || got[0] != list[0] {
					t.Errorf("%s = %#v, want %#v", key, doc[key], want)
				}
			}
		})
	}
}

// The challenge has to sit outside the application's own middleware, or a
// refusal from that middleware would never reach it.
func TestModuleOAuth_ChallengeWrapsApplicationMiddleware(t *testing.T) {
	// A guard that writes the refusal itself rather than returning an error
	// exercises the other commit path through the same mount.
	writeDeny := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Unauthenticated."})
		}
	}
	services := &velapp.Services{}
	r := router.NewV2()
	r.SetServices(services)
	New(whoamiServer(), WithOAuth(oauthConfig()), WithMiddleware(writeDeny)).
		Routes(chain.NewRouting(r, chain.NewMiddlewareStack(services)))

	rec := call(r, whoamiCall)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	const want = `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`
	if got := rec.Header().Get(oauth.HeaderWWWAuthenticate); got != want {
		t.Fatalf("%s = %q, want %q", oauth.HeaderWWWAuthenticate, got, want)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Unauthenticated.") {
		t.Fatalf("body = %s, want the application's own refusal preserved", body)
	}
}
