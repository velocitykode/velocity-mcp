package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/contract"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// The provider relies on the framework's post-bootstrap wiring sweep to
// inject the event dispatcher into the registered server; that only works
// while *server.Server implements contract.EventDispatcherAware. Lock the
// contract here so a server-side regression fails this package too.
var _ contract.EventDispatcherAware = (*server.Server)(nil)

func newTestServer() *server.Server {
	return server.New("provider-test", "0.0.1",
		server.WithTools(
			server.NewTool("hello", "greet").
				WithSchema(func(s *schema.Object) { s.String("name") }).
				HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
					return server.Text("hi"), nil
				}),
		),
	)
}

func TestProvider_RegisterStoresServerTyped(t *testing.T) {
	srv := newTestServer()
	s := &velapp.Services{}

	p := New(srv)
	if err := p.Register(s); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := p.Boot(s); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	if got := server.FromServices(s); got != srv {
		t.Fatalf("FromServices = %p, want %p", got, srv)
	}
}

func TestProvider_RegisterDuplicateErrors(t *testing.T) {
	s := &velapp.Services{}
	if err := New(newTestServer()).Register(s); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := New(newTestServer()).Register(s); err == nil {
		t.Fatal("second Register should error (duplicate *server.Server key)")
	}
}

func TestProvider_RegisterWithoutServerErrors(t *testing.T) {
	if err := New(nil).Register(&velapp.Services{}); err == nil {
		t.Fatal("Register with nil server should error")
	}
}

// mountAndCall registers the provider's routes on a fresh router and drives
// one JSON-RPC request through the mounted endpoint.
func mountAndCall(t *testing.T, p *Provider, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := router.NewV2()
	routing := chain.NewRouting(r, chain.NewMiddlewareStack(&velapp.Services{}))
	p.Routes(routing)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestProvider_RoutesMountsTransportAtDefaultPath(t *testing.T) {
	p := New(newTestServer())
	rec := mountAndCall(t, p, DefaultPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Error  *struct{ Message string } `json:"error"`
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v; body: %s", err, rec.Body.String())
	}
	if envelope.Error != nil {
		t.Fatalf("tools/list errored: %s", envelope.Error.Message)
	}
	if envelope.Result == nil || len(envelope.Result.Tools) != 1 || envelope.Result.Tools[0].Name != "hello" {
		t.Fatalf("tools/list result wrong: %s", rec.Body.String())
	}
}

func TestProvider_WithPathOverridesMount(t *testing.T) {
	p := New(newTestServer(), WithPath("/ai/mcp"))
	rec := mountAndCall(t, p, "/ai/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Default path must NOT be mounted.
	rec = mountAndCall(t, New(newTestServer(), WithPath("/ai/mcp")), DefaultPath, `{}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("default path should not be mounted, got 200")
	}
}

func TestProvider_WithPathEmptyKeepsDefault(t *testing.T) {
	p := New(newTestServer(), WithPath(""))
	rec := mountAndCall(t, p, DefaultPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestProvider_WithMiddlewareWrapsRoute(t *testing.T) {
	called := false
	mw := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			called = true
			return next(c)
		}
	}
	p := New(newTestServer(), WithMiddleware(mw))
	rec := mountAndCall(t, p, DefaultPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("route middleware did not run")
	}
}

func TestProvider_MiddlewareCanRejectBeforeTransport(t *testing.T) {
	deny := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			return router.NewHTTPError(http.StatusUnauthorized, "no token")
		}
	}
	p := New(newTestServer(), WithMiddleware(deny))
	rec := mountAndCall(t, p, DefaultPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestProvider_ShutdownIsNoop(t *testing.T) {
	if err := New(newTestServer()).Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestProvider_CommandsRegistersGenerators(t *testing.T) {
	r := chain.NewCommands()
	New(newTestServer()).Commands(r)

	want := map[string]bool{
		"make:mcp-tool":     false,
		"make:mcp-resource": false,
		"make:mcp-prompt":   false,
	}
	for _, c := range r.All() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("generator %q not registered", name)
		}
	}
}
