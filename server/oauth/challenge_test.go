package oauth

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	clientoauth "github.com/velocitykode/velocity-mcp/client/oauth"
	"github.com/velocitykode/velocity/exceptions"
	"github.com/velocitykode/velocity/router"
)

// ok is a terminal handler that succeeds, standing in for the MCP transport.
func ok(c *router.Context) error { return c.JSON(http.StatusOK, map[string]string{"ok": "yes"}) }

// rejectWritten is a guard that refuses by writing the 401 itself, the shape a
// hand-rolled bearer middleware takes.
func rejectWritten(next router.HandlerFunc) router.HandlerFunc {
	return func(c *router.Context) error { return c.Status(http.StatusUnauthorized) }
}

// rejectJSON is a guard that refuses with a JSON body, the shape velocity's own
// auth middleware takes for an API request.
func rejectJSON(next router.HandlerFunc) router.HandlerFunc {
	return func(c *router.Context) error {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Unauthenticated."})
	}
}

// rejectError is a guard that refuses by returning an error, which the router
// renders after the challenge middleware has already returned.
func rejectError(next router.HandlerFunc) router.HandlerFunc {
	return func(c *router.Context) error { return router.NewHTTPError(http.StatusUnauthorized) }
}

// rejectWrappedError returns a 401 buried inside another error, which the
// router does not unwrap and therefore renders as a 500.
func rejectWrappedError(next router.HandlerFunc) router.HandlerFunc {
	return func(c *router.Context) error {
		return &wrapped{inner: router.NewHTTPError(http.StatusUnauthorized)}
	}
}

type wrapped struct{ inner error }

func (w *wrapped) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrapped) Unwrap() error { return w.inner }

// forbid refuses with a status that is not 401.
func forbid(next router.HandlerFunc) router.HandlerFunc {
	return func(c *router.Context) error { return c.Status(http.StatusForbidden) }
}

// serve mounts a POST route carrying the challenge middleware plus guards and
// drives one request through the real router.
func serve(t *testing.T, cfg Config, path string, guards []router.MiddlewareFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	r := router.NewV2()
	mw := append([]router.MiddlewareFunc{Challenge(cfg)}, guards...)
	r.Post(path, ok).Use(mw...)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func post(path string) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
}

func TestChallenge_AttachedToUnauthorized(t *testing.T) {
	const base = "https://mcp.example.test"

	const metadata = `resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp"`

	tests := []struct {
		name   string
		guards []router.MiddlewareFunc
		path   string
		cfg    Config
		// authorization is the Authorization header the request carries, if any.
		authorization string
		status        int
		want          string
	}{
		{
			// RFC 6750 3: a request that included an access token and failed
			// authentication is told so, which is what makes a client replace
			// the token instead of sending it again.
			name:          "a refused bearer token is reported as invalid",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Bearer expired-token",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", error="invalid_token", ` + metadata + `, scope="mcp:use"`,
		},
		{
			name:          "a refused bearer token when the guard returns an error",
			guards:        []router.MiddlewareFunc{rejectError},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Bearer expired-token",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", error="invalid_token", ` + metadata + `, scope="mcp:use"`,
		},
		{
			name:          "a refused bearer token without published metadata",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base, WithoutResourceMetadata: true},
			authorization: "Bearer expired-token",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", error="invalid_token", scope="mcp:use"`,
		},
		{
			// Authentication scheme names are case-insensitive (RFC 9110 11.1).
			name:          "the bearer scheme in another case",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "bEaReR expired-token",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", error="invalid_token", ` + metadata + `, scope="mcp:use"`,
		},
		{
			// RFC 6750 3: credentials of a scheme this resource does not take
			// are no access token, so nothing has failed that an error code
			// could describe.
			name:          "credentials of another scheme are not a refused token",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Basic dXNlcjpwYXNz",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", ` + metadata + `, scope="mcp:use"`,
		},
		{
			name:          "the bearer scheme with no token after it",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Bearer   ",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", ` + metadata + `, scope="mcp:use"`,
		},
		{
			name:          "the bearer scheme on its own",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Bearer",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", ` + metadata + `, scope="mcp:use"`,
		},
		{
			name:          "a scheme that only starts with bearer",
			guards:        []router.MiddlewareFunc{rejectWritten},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "BearerX expired-token",
			status:        http.StatusUnauthorized,
			want:          `Bearer realm="mcp", ` + metadata + `, scope="mcp:use"`,
		},
		{
			// Only the guard knows whether a 403 is about scope, and which
			// scope: see InsufficientScope.
			name:          "a 403 on a request that presented a token carries no challenge of its own",
			guards:        []router.MiddlewareFunc{forbid},
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Bearer narrow-token",
			status:        http.StatusForbidden,
			want:          "",
		},
		{
			name:          "a successful call with a token carries no challenge",
			path:          "/mcp",
			cfg:           Config{BaseURL: base},
			authorization: "Bearer good-token",
			status:        http.StatusOK,
			want:          "",
		},
		{
			name:   "guard writes the status itself",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp",
			cfg:    Config{BaseURL: base},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			name:   "guard writes a json body",
			guards: []router.MiddlewareFunc{rejectJSON},
			path:   "/mcp",
			cfg:    Config{BaseURL: base},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			name:   "guard returns an http error the router renders later",
			guards: []router.MiddlewareFunc{rejectError},
			path:   "/mcp",
			cfg:    Config{BaseURL: base},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			// The router renders a wrapped error as a 500, so no challenge may
			// be advertised: the header always agrees with the status sent.
			name:   "a wrapped http error the router renders as 500 gets no challenge",
			guards: []router.MiddlewareFunc{rejectWrappedError},
			path:   "/mcp",
			cfg:    Config{BaseURL: base},
			status: http.StatusInternalServerError,
			want:   "",
		},
		{
			name:   "nested resource path is carried into the metadata url",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp/weather",
			cfg:    Config{BaseURL: base},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp/weather", scope="mcp:use"`,
		},
		{
			// The path is carried over as the client spelled it: a resource
			// whose path ends in a slash is not the one whose path does not,
			// and the document it is sent to has to describe the right one.
			name:   "a resource path ending in a slash keeps it",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp/",
			cfg:    Config{BaseURL: base},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp/", scope="mcp:use"`,
		},
		{
			// A request line always carries at least a slash, so it cannot tell
			// the identifier with an empty path from the one with a bare slash;
			// RFC 3986 6.2.3 makes them the same URI, and the unsuffixed
			// document is the one that describes it.
			name:   "root resource addresses the unsuffixed document",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/",
			cfg:    Config{BaseURL: base},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource", scope="mcp:use"`,
		},
		{
			name:   "configured scope replaces the default",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp",
			cfg:    Config{BaseURL: base, Scope: "provisioning"},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="provisioning"`,
		},
		{
			// RFC 6750 3: a request that came without credentials has failed at
			// nothing, so no error code is stated. What is left to say without
			// a metadata document is the realm and the scope to ask for.
			name:   "without published metadata the realm and the scope are what is advertised",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp",
			cfg:    Config{BaseURL: base, WithoutResourceMetadata: true},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", scope="mcp:use"`,
		},
		{
			// A deployment whose metadata document is served by another host
			// pins the URL, and the derivation must not be applied to it: no
			// origin substituted, no request path appended.
			name:   "a pinned metadata url is advertised verbatim",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp",
			cfg: Config{
				BaseURL:             base,
				ResourceMetadataURL: "https://id.example.test/.well-known/oauth-protected-resource",
				Scope:               "provisioning",
			},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="https://id.example.test/.well-known/oauth-protected-resource", scope="provisioning"`,
		},
		{
			// The same deployment publishes no document of its own, and the
			// pinned URL still wins: it is what the client can actually fetch.
			name:   "a pinned url outranks the declaration that none is published",
			guards: []router.MiddlewareFunc{rejectWritten},
			path:   "/mcp",
			cfg: Config{
				ResourceMetadataURL:     "http://localhost:4010/.well-known/oauth-protected-resource",
				WithoutResourceMetadata: true,
			},
			status: http.StatusUnauthorized,
			want:   `Bearer realm="mcp", resource_metadata="http://localhost:4010/.well-known/oauth-protected-resource", scope="mcp:use"`,
		},
		{
			name:   "a non-401 refusal carries no challenge",
			guards: []router.MiddlewareFunc{forbid},
			path:   "/mcp",
			cfg:    Config{BaseURL: base},
			status: http.StatusForbidden,
			want:   "",
		},
		{
			name:   "a successful call carries no challenge",
			guards: nil,
			path:   "/mcp",
			cfg:    Config{BaseURL: base},
			status: http.StatusOK,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := post(tt.path)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			w := serve(t, tt.cfg, tt.path, tt.guards, req)

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if got := w.Header().Get(HeaderWWWAuthenticate); got != tt.want {
				t.Fatalf("%s = %q, want %q", HeaderWWWAuthenticate, got, tt.want)
			}
		})
	}
}

// The package authenticates nothing: whether a token is genuine, unexpired and
// issued for this resource (the audience check the MCP authorization
// specification requires of a server) is decided by the guard the application
// puts behind this middleware, and by nothing here. The middleware takes no part
// in that decision in either direction. It never admits a request because of the
// token it carries, and never refuses one the guard let through, so the outcome
// of every request is the guard's alone.
func TestChallenge_TakesNoPartInAuthentication(t *testing.T) {
	// A token whose payload names another resource as its audience, one that is
	// not a token at all, and none.
	tokens := map[string]string{
		"a token issued for another resource": "Bearer eyJhbGciOiJub25lIn0.eyJhdWQiOiJodHRwczovL290aGVyLmV4YW1wbGUudGVzdC9tY3AifQ.",
		"something that is not a token":       "Bearer \x7f!!",
		"no credentials":                      "",
	}
	admit := func(next router.HandlerFunc) router.HandlerFunc { return next }
	guards := []struct {
		name   string
		guard  router.MiddlewareFunc
		status int
	}{
		{name: "a guard that admits", guard: admit, status: http.StatusOK},
		{name: "a guard that refuses", guard: rejectWritten, status: http.StatusUnauthorized},
		{name: "a guard that forbids", guard: forbid, status: http.StatusForbidden},
	}

	for _, g := range guards {
		for name, authorization := range tokens {
			t.Run(g.name+"/"+name, func(t *testing.T) {
				req := post("/mcp")
				if authorization != "" {
					req.Header.Set("Authorization", authorization)
				}
				w := serve(t, Config{BaseURL: "https://mcp.example.test"}, "/mcp", []router.MiddlewareFunc{g.guard}, req)

				if w.Code != g.status {
					t.Fatalf("status = %d, want the guard's %d", w.Code, g.status)
				}
				if challenged := w.Header().Get(HeaderWWWAuthenticate) != ""; challenged != (g.status == http.StatusUnauthorized) {
					t.Fatalf("challenged = %v on a %d", challenged, g.status)
				}
			})
		}
	}
}

// A guard that reads the token may well remove the header once it has, the way
// a middleware that swaps credentials for an identity does. Whether the request
// came with a token is a fact about the request as it arrived, so the challenge
// does not change with what the chain did to it afterwards.
func TestChallenge_RemembersTheTokenAGuardStripped(t *testing.T) {
	strip := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			c.Request.Header.Del("Authorization")
			return c.Status(http.StatusUnauthorized)
		}
	}

	req := post("/mcp")
	req.Header.Set("Authorization", "Bearer expired-token")
	w := serve(t, Config{BaseURL: "https://mcp.example.test"}, "/mcp", []router.MiddlewareFunc{strip}, req)

	const want = `Bearer realm="mcp", error="invalid_token", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`
	if got := w.Header().Get(HeaderWWWAuthenticate); got != want {
		t.Fatalf("%s = %q, want %q", HeaderWWWAuthenticate, got, want)
	}
}

// A token that is valid and does not reach far enough is a 403, and the MCP
// authorization specification has the server say which scope the call needs and
// where to get it (RFC 6750 3.1 insufficient_scope). Only the guard knows that,
// so it states it through InsufficientScope and refuses however it refuses
// anything else; the challenge middleware leaves what it set alone.
func TestInsufficientScope_ChallengesA403(t *testing.T) {
	const base = "https://mcp.example.test"
	const metadata = `resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp"`

	refuse := map[string]func(c *router.Context) error{
		"written": func(c *router.Context) error {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient_scope"})
		},
		"returned": func(c *router.Context) error { return router.NewHTTPError(http.StatusForbidden) },
	}

	tests := []struct {
		name  string
		cfg   Config
		scope string
		want  string
	}{
		{
			name:  "the scope the call needs",
			cfg:   Config{BaseURL: base},
			scope: "files:write",
			want:  `Bearer realm="mcp", error="insufficient_scope", ` + metadata + `, scope="files:write"`,
		},
		{
			name:  "several scopes",
			cfg:   Config{BaseURL: base},
			scope: "files:read files:write",
			want:  `Bearer realm="mcp", error="insufficient_scope", ` + metadata + `, scope="files:read files:write"`,
		},
		{
			name: "no scope named means the scope the resource requires",
			cfg:  Config{BaseURL: base, Scope: "provisioning"},
			want: `Bearer realm="mcp", error="insufficient_scope", ` + metadata + `, scope="provisioning"`,
		},
		{
			name:  "without published metadata",
			cfg:   Config{BaseURL: base, WithoutResourceMetadata: true},
			scope: "files:write",
			want:  `Bearer realm="mcp", error="insufficient_scope", scope="files:write"`,
		},
		{
			name:  "a pinned metadata url",
			cfg:   Config{BaseURL: base, ResourceMetadataURL: "https://id.example.test/.well-known/oauth-protected-resource"},
			scope: "files:write",
			want:  `Bearer realm="mcp", error="insufficient_scope", resource_metadata="https://id.example.test/.well-known/oauth-protected-resource", scope="files:write"`,
		},
		{
			name:  "a scope that tries to close the quoted-string",
			cfg:   Config{BaseURL: base},
			scope: "files:write\", error=\"invalid_token\r\nX-Injected: 1",
			want:  `Bearer realm="mcp", error="insufficient_scope", ` + metadata + `, scope="files:write\", error=\"invalid_tokenX-Injected: 1"`,
		},
	}

	for _, tt := range tests {
		for how, answer := range refuse {
			t.Run(tt.name+"/"+how, func(t *testing.T) {
				guard := func(next router.HandlerFunc) router.HandlerFunc {
					return func(c *router.Context) error {
						InsufficientScope(c, tt.cfg, tt.scope)
						return answer(c)
					}
				}
				req := post("/mcp")
				req.Header.Set("Authorization", "Bearer narrow-token")
				w := serve(t, tt.cfg, "/mcp", []router.MiddlewareFunc{guard}, req)

				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", w.Code)
				}
				if got := w.Header().Get(HeaderWWWAuthenticate); got != tt.want {
					t.Fatalf("%s = %q, want %q", HeaderWWWAuthenticate, got, tt.want)
				}
				if w.Header().Get("X-Injected") != "" {
					t.Fatal("the scope split the response into another header")
				}
			})
		}
	}
}

func TestInsufficientScope_WithoutAResponseBehindIt(t *testing.T) {
	// Nothing to set the header on is nothing to do, not a panic.
	InsufficientScope(nil, Config{}, "files:write")
	InsufficientScope(&router.Context{}, Config{}, "files:write")
}

// The resource path arrives from the URL, so it can hold anything a URL can
// carry. Whatever goes into the advertised metadata URL has to be encoded, and
// encoded the same way the document's own resource identifier is, or the
// challenge and the document disagree about the same resource.
func TestChallenge_EscapesTheResourcePath(t *testing.T) {
	r := router.NewV2()
	r.Post("/mcp/{rest:.*}", ok).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}), rejectWritten)
	// The same config also serves the document, so both sides can be compared.
	Routes(r, Config{BaseURL: "https://mcp.example.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, `/mcp/a%20b/%22quoted%22`, strings.NewReader("{}")))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	const wantURL = "https://mcp.example.test/.well-known/oauth-protected-resource/mcp/a%20b/%22quoted%22"
	want := `Bearer realm="mcp", resource_metadata="` + wantURL + `", scope="mcp:use"`
	if got := w.Header().Get(HeaderWWWAuthenticate); got != want {
		t.Fatalf("%s = %q, want %q", HeaderWWWAuthenticate, got, want)
	}

	// And the URL it named resolves to the document describing that resource.
	doc := httptest.NewRecorder()
	r.ServeHTTP(doc, httptest.NewRequest(http.MethodGet, `/.well-known/oauth-protected-resource/mcp/a%20b/%22quoted%22`, nil))
	if doc.Code != http.StatusOK {
		t.Fatalf("document status = %d, want 200: %s", doc.Code, doc.Body.String())
	}
	var body struct {
		Resource string `json:"resource"`
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", doc.Body.String(), err)
	}
	if body.Resource != `https://mcp.example.test/mcp/a%20b/%22quoted%22` {
		t.Fatalf("resource = %q, want the same encoding the challenge used", body.Resource)
	}
}

func TestChallenge_OriginDerivedFromRequest(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*http.Request)
		want string
	}{
		{
			name: "plain http request",
			mut:  func(r *http.Request) { r.Host = "mcp.internal:8080" },
			want: `Bearer realm="mcp", resource_metadata="http://mcp.internal:8080/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			name: "tls request",
			mut: func(r *http.Request) {
				r.Host = "mcp.example.test"
				r.TLS = &tls.ConnectionState{}
			},
			want: `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			name: "a forwarded proto header is never trusted",
			mut: func(r *http.Request) {
				r.Host = "mcp.example.test"
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			want: `Bearer realm="mcp", resource_metadata="http://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			name: "a host carrying quotes cannot break out of the auth-param",
			mut:  func(r *http.Request) { r.Host = `evil"; scope="admin` },
			want: `Bearer realm="mcp", resource_metadata="http://evil\"; scope=\"admin/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
		{
			name: "control characters in the host are dropped, not emitted",
			mut:  func(r *http.Request) { r.Host = "evil\r\nX-Injected: 1" },
			want: `Bearer realm="mcp", resource_metadata="http://evilX-Injected: 1/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := post("/mcp")
			tt.mut(req)

			w := serve(t, Config{}, "/mcp", []router.MiddlewareFunc{rejectWritten}, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if got := w.Header().Get(HeaderWWWAuthenticate); got != tt.want {
				t.Fatalf("%s = %q, want %q", HeaderWWWAuthenticate, got, tt.want)
			}
		})
	}
}

// An application that installs its own error renderer (velocity's exceptions
// handler is the idiomatic one) turns a guard's refusal into a response long
// after this middleware has returned, and writes it through the router context.
// The challenge has to survive that, or every application with a custom error
// handler silently loses it.
func TestChallenge_SurvivesACustomErrorRenderer(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"an http error the renderer maps itself", router.NewHTTPError(http.StatusUnauthorized)},
		{"an unauthorized exception", exceptions.NewUnauthorizedHttpException()},
		{"a wrapped unauthorized exception", errors.Join(exceptions.NewUnauthorizedHttpException())},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := router.NewV2()
			// A renderer that reads the status off the error and answers
			// through the context, the shape an exceptions handler takes.
			r.ErrorHandler = func(c *router.Context, err error) {
				status := http.StatusInternalServerError
				var httpErr *router.HTTPError
				var coded interface{ GetStatusCode() int }
				switch {
				case errors.As(err, &httpErr):
					status = httpErr.Code
				case errors.As(err, &coded):
					status = coded.GetStatusCode()
				}
				_ = c.JSON(status, map[string]string{"message": "Unauthenticated."})
			}
			reject := func(next router.HandlerFunc) router.HandlerFunc {
				return func(c *router.Context) error { return tt.err }
			}
			r.Post("/mcp", ok).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}), reject)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, post("/mcp"))

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
			}
			const want = `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp", scope="mcp:use"`
			if got := w.Header().Get(HeaderWWWAuthenticate); got != want {
				t.Fatalf("%s = %q, want %q", HeaderWWWAuthenticate, got, want)
			}
			if !strings.Contains(w.Body.String(), "Unauthenticated.") {
				t.Fatalf("body = %s, want the renderer's own body", w.Body.String())
			}
		})
	}
}

// A custom renderer that answers something other than a 401 must not pick up a
// challenge on the way: the header always agrees with the status sent.
func TestChallenge_CustomRendererNon401GetsNoChallenge(t *testing.T) {
	r := router.NewV2()
	r.ErrorHandler = func(c *router.Context, err error) {
		_ = c.JSON(http.StatusServiceUnavailable, map[string]string{"message": "later"})
	}
	reject := func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error { return router.NewHTTPError(http.StatusUnauthorized) }
	}
	r.Post("/mcp", ok).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}), reject)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, post("/mcp"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if got := w.Header().Get(HeaderWWWAuthenticate); got != "" {
		t.Fatalf("%s = %q, want none on a non-401", HeaderWWWAuthenticate, got)
	}
}

// A guard that sets its own challenge keeps it, through either commit path: it
// knows which credential it rejected, and the resource_metadata URL, scope and
// error code it advertises are the ones the client has to act on. The challenge
// this package builds covers a 401 that arrives with nothing at all.
func TestChallenge_PreservesAGuardsOwnValue(t *testing.T) {
	const guardValue = `Bearer realm="other", resource_metadata="https://auth.example.test/.well-known/oauth-protected-resource", scope="admin", error="insufficient_scope"`

	tests := []struct {
		name  string
		guard router.MiddlewareFunc
	}{
		{
			name: "the guard writes the refusal itself",
			guard: func(next router.HandlerFunc) router.HandlerFunc {
				return func(c *router.Context) error {
					c.SetHeader(HeaderWWWAuthenticate, guardValue)
					return c.Status(http.StatusUnauthorized)
				}
			},
		},
		{
			name: "the guard returns the refusal as an error",
			guard: func(next router.HandlerFunc) router.HandlerFunc {
				return func(c *router.Context) error {
					c.SetHeader(HeaderWWWAuthenticate, guardValue)
					return router.NewHTTPError(http.StatusUnauthorized)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serve(t, Config{BaseURL: "https://mcp.example.test"}, "/mcp",
				[]router.MiddlewareFunc{tt.guard}, post("/mcp"))

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if got := w.Header().Get(HeaderWWWAuthenticate); got != guardValue {
				t.Fatalf("%s = %q, want the guard's own challenge %q", HeaderWWWAuthenticate, got, guardValue)
			}
			if values := w.Header().Values(HeaderWWWAuthenticate); len(values) != 1 {
				t.Fatalf("%s carried %d values, want exactly one", HeaderWWWAuthenticate, len(values))
			}
		})
	}
}

// A custom error renderer that answers a 401 with its own challenge keeps it
// too: the anticipated challenge is only ever a stand-in for one nobody set.
func TestChallenge_PreservesACustomRenderersOwnValue(t *testing.T) {
	const rendered = `Bearer realm="other", error="insufficient_scope", scope="admin"`

	r := router.NewV2()
	r.ErrorHandler = func(c *router.Context, err error) {
		c.SetHeader(HeaderWWWAuthenticate, rendered)
		_ = c.JSON(http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
	}
	r.Post("/mcp", ok).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}), rejectError)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, post("/mcp"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderWWWAuthenticate); got != rendered {
		t.Fatalf("%s = %q, want the renderer's own challenge %q", HeaderWWWAuthenticate, got, rendered)
	}
	if values := w.Header().Values(HeaderWWWAuthenticate); len(values) != 1 {
		t.Fatalf("%s carried %d values, want exactly one", HeaderWWWAuthenticate, len(values))
	}
}

// A handler that commits a success and then tries to refuse cannot retroactively
// gain a challenge: the status was already sent.
func TestChallenge_NoChallengeAfterTheStatusIsCommitted(t *testing.T) {
	r := router.NewV2()
	r.Post("/mcp", func(c *router.Context) error {
		if err := c.JSON(http.StatusOK, map[string]string{"ok": "yes"}); err != nil {
			return err
		}
		c.Response.WriteHeader(http.StatusUnauthorized)
		return nil
	}).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, post("/mcp"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the first status written", w.Code)
	}
	if got := w.Header().Get(HeaderWWWAuthenticate); got != "" {
		t.Fatalf("%s = %q, want none", HeaderWWWAuthenticate, got)
	}
}

// hijackRecorder is a response writer that supports the optional interfaces a
// streaming or upgrading handler reaches for, so what the wrapper forwards can
// be observed from a real route.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
	pushed   string
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func (h *hijackRecorder) Push(target string, _ *http.PushOptions) error {
	h.pushed = target
	return nil
}

// Wrapping the response writer must not cost a handler a capability it would
// otherwise have: an upgrade or a server push has to reach the real writer.
func TestChallenge_ForwardsWriterCapabilities(t *testing.T) {
	var (
		gotHijacker, gotPusher bool
		pushErr, hijackErr     error
		deadlineErr            error
	)

	r := router.NewV2()
	r.Post("/mcp", func(c *router.Context) error {
		p, isPusher := c.Response.(http.Pusher)
		gotPusher = isPusher
		if isPusher {
			pushErr = p.Push("/asset.js", nil)
		}
		h, isHijacker := c.Response.(http.Hijacker)
		gotHijacker = isHijacker
		if isHijacker {
			_, _, hijackErr = h.Hijack()
		}
		// A capability no wrapper in the chain implements has to be searched
		// for by unwrapping, which is how http.ResponseController reaches the
		// real writer. It is not supported here, but the answer has to come
		// from the bottom of the chain rather than from the wrapper.
		deadlineErr = http.NewResponseController(c.Response).SetReadDeadline(time.Time{})
		return nil
	}).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}))

	inner := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(inner, post("/mcp"))

	if !gotPusher || !gotHijacker {
		t.Fatalf("handler saw pusher = %v, hijacker = %v, want both", gotPusher, gotHijacker)
	}
	if pushErr != nil || inner.pushed != "/asset.js" {
		t.Fatalf("push err = %v, target = %q, want it forwarded", pushErr, inner.pushed)
	}
	if hijackErr != nil || !inner.hijacked {
		t.Fatalf("hijack err = %v, forwarded = %v, want it forwarded", hijackErr, inner.hijacked)
	}
	if !errors.Is(deadlineErr, http.ErrNotSupported) {
		t.Fatalf("read deadline err = %v, want it unwrapped down to http.ErrNotSupported", deadlineErr)
	}
}

// A writer that supports none of the optional interfaces has to be reported
// honestly through the wrapper rather than faked, and must not panic.
func TestChallenge_ReportsUnsupportedWriterCapabilities(t *testing.T) {
	var pushErr, hijackErr error

	r := router.NewV2()
	r.Post("/mcp", func(c *router.Context) error {
		// The router's own wrapper is between the handler and ours, and both
		// report the same way, so reaching the bottom is what is asserted.
		if p, ok := c.Response.(http.Pusher); ok {
			pushErr = p.Push("/asset.js", nil)
		}
		if h, ok := c.Response.(http.Hijacker); ok {
			_, _, hijackErr = h.Hijack()
		}
		return c.Status(http.StatusOK)
	}).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}))

	r.ServeHTTP(httptest.NewRecorder(), post("/mcp"))

	if !errors.Is(pushErr, http.ErrNotSupported) {
		t.Fatalf("push err = %v, want http.ErrNotSupported", pushErr)
	}
	if !errors.Is(hijackErr, http.ErrNotSupported) {
		t.Fatalf("hijack err = %v, want http.ErrNotSupported", hijackErr)
	}
}

// Driven outside a router, with no request to derive an origin from, the
// middleware has nothing to build a URL out of and must still run: a panic here
// would take down whatever drove it.
func TestChallenge_WithoutARequestBehindIt(t *testing.T) {
	ran := false
	want := errors.New("from the chain")

	err := Challenge(Config{})(func(c *router.Context) error {
		ran = true
		return want
	})(&router.Context{})

	if !ran {
		t.Fatal("the chain did not run")
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// The value builders are the pieces an application reuses when it writes the
// refusal itself, so their forms are pinned independently of the middleware.
// Which one applies is RFC 6750 3: no error code for a request that came without
// credentials, invalid_token for one whose token was refused, insufficient_scope
// for one whose token does not reach far enough.
func TestChallengeValue_Forms(t *testing.T) {
	const url = "https://mcp.example.test/.well-known/oauth-protected-resource/mcp"

	tests := []struct {
		name        string
		build       func(metadataURL, scope string) string
		metadataURL string
		scope       string
		want        string
	}{
		{
			name:        "metadata and scope",
			build:       ChallengeValue,
			metadataURL: url,
			scope:       "mcp:use",
			want:        `Bearer realm="mcp", resource_metadata="` + url + `", scope="mcp:use"`,
		},
		{
			name:        "metadata without a scope",
			build:       ChallengeValue,
			metadataURL: "https://mcp.example.test/.well-known/oauth-protected-resource",
			want:        `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource"`,
		},
		{
			name:  "a scope without metadata",
			build: ChallengeValue,
			scope: "mcp:use",
			want:  `Bearer realm="mcp", scope="mcp:use"`,
		},
		{
			name:  "nothing at all still names the realm",
			build: ChallengeValue,
			want:  `Bearer realm="mcp"`,
		},
		{
			name:        "a refused token",
			build:       InvalidTokenChallengeValue,
			metadataURL: url,
			scope:       "mcp:use",
			want:        `Bearer realm="mcp", error="invalid_token", resource_metadata="` + url + `", scope="mcp:use"`,
		},
		{
			name:  "a refused token and nothing else to say",
			build: InvalidTokenChallengeValue,
			want:  `Bearer realm="mcp", error="invalid_token"`,
		},
		{
			name:        "a token that lacks scope",
			build:       InsufficientScopeChallengeValue,
			metadataURL: url,
			scope:       "files:read files:write",
			want:        `Bearer realm="mcp", error="insufficient_scope", resource_metadata="` + url + `", scope="files:read files:write"`,
		},
		{
			name:  "a token that lacks scope, without metadata",
			build: InsufficientScopeChallengeValue,
			scope: "files:write",
			want:  `Bearer realm="mcp", error="insufficient_scope", scope="files:write"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.build(tt.metadataURL, tt.scope); got != tt.want {
				t.Fatalf("value for (%q, %q) = %q, want %q", tt.metadataURL, tt.scope, got, tt.want)
			}
		})
	}
}

// A challenge value must never contain a bare CR or LF: a header carrying one
// splits the response.
func TestChallengeValue_NeverCarriesLineBreaks(t *testing.T) {
	hostile := "https://host/\r\nX-Injected: 1\x00/path\"\\"
	got := ChallengeValue(hostile, "sc\rope\n")

	if strings.ContainsAny(got, "\r\n\x00") {
		t.Fatalf("challenge value carries a control character: %q", got)
	}
	if !strings.HasPrefix(got, `Bearer realm="mcp", resource_metadata="`) {
		t.Fatalf("challenge value = %q, want the resource_metadata form", got)
	}
	if !strings.Contains(got, `\"`) || !strings.Contains(got, `\\`) {
		t.Fatalf("challenge value = %q, want the quote and backslash escaped", got)
	}
}

// The wrapper the middleware installs must not cost the handler the streaming
// it needs for the event-stream MCP transport.
func TestChallenge_PreservesFlusher(t *testing.T) {
	var flushed bool

	r := router.NewV2()
	r.Post("/mcp", func(c *router.Context) error {
		f, isFlusher := c.Response.(http.Flusher)
		if !isFlusher {
			t.Error("response writer lost http.Flusher through the challenge middleware")
			return nil
		}
		if _, err := c.Response.Write([]byte("data: {}\n\n")); err != nil {
			return err
		}
		f.Flush()
		flushed = true
		return nil
	}).Use(Challenge(Config{BaseURL: "https://mcp.example.test"}))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, post("/mcp"))

	if !flushed {
		t.Fatal("handler did not reach the flush")
	}
	if w.Body.String() != "data: {}\n\n" {
		t.Fatalf("body = %q, want the streamed frame", w.Body.String())
	}
	if got := w.Header().Get(HeaderWWWAuthenticate); got != "" {
		t.Fatalf("%s = %q, want none on a 200", HeaderWWWAuthenticate, got)
	}
}

// One middleware instance serves every MCP route concurrently, so the challenge
// must be derived per request and never shared through the closure.
func TestChallenge_ConcurrentRequestsGetTheirOwnResource(t *testing.T) {
	cfg := Config{BaseURL: "https://mcp.example.test"}

	r := router.NewV2()
	challenge := Challenge(cfg)
	for _, path := range []string{"/mcp", "/mcp/weather", "/tenants/a/mcp"} {
		r.Post(path, ok).Use(challenge, rejectWritten)
	}

	paths := []string{"/mcp", "/mcp/weather", "/tenants/a/mcp"}
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		path := paths[i%len(paths)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, post(path))

			want := `Bearer realm="mcp", resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource` + path + `", scope="mcp:use"`
			if got := w.Header().Get(HeaderWWWAuthenticate); got != want {
				t.Errorf("%s for %s = %q, want %q", HeaderWWWAuthenticate, path, got, want)
			}
		}()
	}
	wg.Wait()
}

// The challenge this package emits has to be readable by the client half of
// this SDK, which is what actually turns a 401 into an OAuth run. Parsing it
// back is the contract between the two, so it is asserted here rather than
// assumed.
func TestChallenge_ReadableByTheClient(t *testing.T) {
	w := serve(t, Config{BaseURL: "https://mcp.example.test", Scope: "provisioning"},
		"/mcp", []router.MiddlewareFunc{rejectWritten}, post("/mcp"))

	parsed := clientoauth.ParseChallenge(w.Header().Get(HeaderWWWAuthenticate))

	const wantURL = "https://mcp.example.test/.well-known/oauth-protected-resource/mcp"
	if parsed.ResourceMetadataURL != wantURL {
		t.Errorf("resource_metadata = %q, want %q", parsed.ResourceMetadataURL, wantURL)
	}
	if parsed.Scope != "provisioning" {
		t.Errorf("scope = %q, want %q", parsed.Scope, "provisioning")
	}
	if parsed.Error != "" {
		t.Errorf("error = %q, want none when metadata is advertised", parsed.Error)
	}

	// Without a document to point at, the scope still reaches the client.
	w = serve(t, Config{BaseURL: "https://mcp.example.test", WithoutResourceMetadata: true},
		"/mcp", []router.MiddlewareFunc{rejectWritten}, post("/mcp"))

	parsed = clientoauth.ParseChallenge(w.Header().Get(HeaderWWWAuthenticate))
	if parsed.ResourceMetadataURL != "" {
		t.Errorf("resource_metadata = %q, want none", parsed.ResourceMetadataURL)
	}
	if parsed.Scope != "mcp:use" || parsed.Error != "" {
		t.Errorf("scope = %q error = %q, want mcp:use and no error for a request without credentials", parsed.Scope, parsed.Error)
	}

	// A refused token is told apart from a missing one, and loses none of the
	// rest of the challenge for it.
	refused := post("/mcp")
	refused.Header.Set("Authorization", "Bearer expired-token")
	w = serve(t, Config{BaseURL: "https://mcp.example.test", Scope: "provisioning"},
		"/mcp", []router.MiddlewareFunc{rejectWritten}, refused)

	parsed = clientoauth.ParseChallenge(w.Header().Get(HeaderWWWAuthenticate))
	if parsed.Error != "invalid_token" || parsed.ResourceMetadataURL != wantURL || parsed.Scope != "provisioning" {
		t.Errorf("challenge = %+v, want invalid_token with %q and provisioning", parsed, wantURL)
	}

	// And so is a token that lacks scope, with the scope the call needs.
	parsed = clientoauth.ParseChallenge(InsufficientScopeChallengeValue(wantURL, "files:write"))
	if parsed.Error != "insufficient_scope" || parsed.ResourceMetadataURL != wantURL || parsed.Scope != "files:write" {
		t.Errorf("challenge = %+v, want insufficient_scope with %q and files:write", parsed, wantURL)
	}
}

// FuzzChallengeValue drives arbitrary values through the header encoder. The
// resource metadata URL is attacker-reachable (it is built from the Host header
// when no BaseURL is configured), so whatever comes out has to be a well-formed
// RFC 9110 quoted-string carrying exactly the auth-params this package emits:
// no line break to split the response, no extra parameter smuggled in, and the
// value the client reads back is the one that went in.
func FuzzChallengeValue(f *testing.F) {
	f.Add("https://mcp.example.test/.well-known/oauth-protected-resource/mcp", "mcp:use")
	f.Add(`https://evil"; scope="admin`, "mcp:use")
	f.Add("https://x.test/\r\nX-Injected: 1", "sc\rope\n")
	f.Add(`back\slash`, `sco"pe`)
	f.Add("", "mcp:use")
	f.Add("https://x.test", "")
	f.Add("\x00\x7f", "\x00")
	f.Add("ünïcödé://ok", "scope ünï")

	builders := []struct {
		build func(metadataURL, scope string) string
		// code is the error the builder states, or "" for none.
		code string
	}{
		{build: ChallengeValue},
		{build: InvalidTokenChallengeValue, code: "invalid_token"},
		{build: InsufficientScopeChallengeValue, code: "insufficient_scope"},
	}

	f.Fuzz(func(t *testing.T, metadataURL, scope string) {
		for _, builder := range builders {
			got := builder.build(metadataURL, scope)

			if strings.ContainsAny(got, "\r\n\x00") {
				t.Fatalf("value for (%q, %q) = %q, which can split the response", metadataURL, scope, got)
			}
			params, err := parseAuthParams(got)
			if err != nil {
				t.Fatalf("value for (%q, %q) = %q: %v", metadataURL, scope, got, err)
			}

			// Exactly the parameters this call stated, each with the value that
			// went in, and nothing a value could have smuggled in beside them.
			want := map[string]string{"realm": challengeRealm}
			if builder.code != "" {
				want["error"] = builder.code
			}
			if v := stripControl(metadataURL); v != "" {
				want["resource_metadata"] = v
			}
			if v := stripControl(scope); v != "" {
				want["scope"] = v
			}
			if !maps.Equal(params, want) {
				t.Fatalf("params = %v, want %v (in %q)", params, want, got)
			}
		}
	})
}

// stripControl removes the bytes a quoted-string may not carry, which is what
// the encoder drops rather than escapes. It works byte by byte, like the
// encoder: a header value is bytes, and a value that is not valid UTF-8 must
// still come back unchanged apart from those drops.
func stripControl(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if ch := v[i]; ch >= 0x20 && ch != 0x7f {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// parseAuthParams decodes a "Bearer k=\"v\", ..." challenge the way RFC 9110
// 5.6.4 defines a quoted-string, including backslash escapes, so the encoder is
// checked against the grammar rather than against a lenient reader.
func parseAuthParams(value string) (map[string]string, error) {
	rest, ok := strings.CutPrefix(value, "Bearer ")
	if !ok {
		return nil, errors.New("challenge does not start with the Bearer scheme")
	}

	params := map[string]string{}
	for {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return nil, fmt.Errorf("auth-param %q has no value", rest)
		}
		key := rest[:eq]
		if key == "" || strings.ContainsAny(key, " \t\",\\") {
			return nil, fmt.Errorf("auth-param name %q is not a token", key)
		}
		if _, dup := params[key]; dup {
			return nil, fmt.Errorf("auth-param %q appears twice", key)
		}
		rest = rest[eq+1:]
		if !strings.HasPrefix(rest, `"`) {
			return nil, fmt.Errorf("auth-param %q is not a quoted-string", key)
		}

		var decoded strings.Builder
		i := 1
		closed := false
		for i < len(rest) {
			switch ch := rest[i]; {
			case ch == '\\':
				if i+1 >= len(rest) {
					return nil, errors.New("quoted-string ends inside an escape")
				}
				decoded.WriteByte(rest[i+1])
				i += 2
			case ch == '"':
				closed = true
				i++
			case ch < 0x20 || ch == 0x7f:
				return nil, fmt.Errorf("quoted-string carries control byte %#x", ch)
			default:
				decoded.WriteByte(ch)
				i++
			}
			if closed {
				break
			}
		}
		if !closed {
			return nil, errors.New("quoted-string is never closed")
		}
		params[key] = decoded.String()

		rest = rest[i:]
		if rest == "" {
			return params, nil
		}
		var found bool
		if rest, found = strings.CutPrefix(rest, ", "); !found {
			return nil, fmt.Errorf("auth-params are not comma separated at %q", rest)
		}
	}
}

// preCommitHooker is the capability velocity's save-at-end session middleware
// looks for on the response writer, asserted directly on it with no unwrapping.
// A wrapper installed between the framework's writer and the handler has to
// carry the method itself, or that middleware silently loses its pre-commit
// save and emits Set-Cookie into headers that have already gone to the client.
type preCommitHooker interface {
	BeforeFirstWrite(fn func())
}

// saveCookieAtEnd models the save-at-end middleware: it registers a pre-commit
// hook that writes a cookie, with a post-handler fallback for a handler that
// commits nothing, and writes the cookie at most once either way.
func saveCookieAtEnd(cookie *http.Cookie) router.MiddlewareFunc {
	return func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			var once sync.Once
			save := func() { once.Do(func() { http.SetCookie(c.Response, cookie) }) }

			if h, ok := c.Response.(preCommitHooker); ok {
				h.BeforeFirstWrite(save)
			}
			err := next(c)
			save()
			return err
		}
	}
}

// writeBody is a terminal handler that commits an implicit 200 by writing a
// body straight to the response writer, without going through c.JSON.
func writeBody(c *router.Context) error {
	_, err := c.Response.Write([]byte("ok"))
	return err
}

// A middleware installed under Challenge must still be able to put headers on
// the response as it commits. The cookie is read back off a real HTTP response,
// which is where the difference shows: a header added after the status line has
// gone out is dropped by the transport rather than delivered late.
func TestChallenge_ForwardsPreCommitHook(t *testing.T) {
	cookie := &http.Cookie{Name: "mcp_session", Value: "persisted", Path: "/"}

	tests := []struct {
		name       string
		guards     []router.MiddlewareFunc
		handler    router.HandlerFunc
		wantStatus int
	}{
		{
			name:       "handler answers with a body",
			handler:    ok,
			wantStatus: http.StatusOK,
		},
		{
			name:       "handler writes the body itself",
			handler:    writeBody,
			wantStatus: http.StatusOK,
		},
		{
			name:       "guard writes a bare 401",
			guards:     []router.MiddlewareFunc{rejectWritten},
			handler:    ok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "guard writes a 401 with a body",
			guards:     []router.MiddlewareFunc{rejectJSON},
			handler:    ok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "guard returns a 401 the router renders",
			guards:     []router.MiddlewareFunc{rejectError},
			handler:    ok,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "guard refuses with another status",
			guards:     []router.MiddlewareFunc{forbid},
			handler:    ok,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := router.NewV2()
			mw := append([]router.MiddlewareFunc{Challenge(Config{}), saveCookieAtEnd(cookie)}, tt.guards...)
			r.Post("/mcp", tt.handler).Use(mw...)

			srv := httptest.NewServer(r)
			defer srv.Close()

			resp, err := srv.Client().Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			var got *http.Cookie
			for _, c := range resp.Cookies() {
				if c.Name == cookie.Name {
					got = c
				}
			}
			if got == nil {
				t.Fatalf("Set-Cookie %q missing from %v", cookie.Name, resp.Header.Values("Set-Cookie"))
			}
			if got.Value != cookie.Value {
				t.Errorf("cookie value = %q, want %q", got.Value, cookie.Value)
			}
			if n := len(resp.Header.Values("Set-Cookie")); n != 1 {
				t.Errorf("Set-Cookie count = %d, want 1", n)
			}

			// The challenge still has to be on the refusals, and off
			// everything else, with the hook in the way.
			challenge := resp.Header.Get(HeaderWWWAuthenticate)
			if tt.wantStatus == http.StatusUnauthorized {
				if !strings.HasPrefix(challenge, `Bearer realm="mcp"`) {
					t.Errorf("%s = %q, want a bearer challenge", HeaderWWWAuthenticate, challenge)
				}
			} else if challenge != "" {
				t.Errorf("%s = %q, want none", HeaderWWWAuthenticate, challenge)
			}
		})
	}
}
