package oauth

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/velocitykode/velocity/contract"
	"github.com/velocitykode/velocity/router"
)

// HeaderWWWAuthenticate is the response header carrying the bearer challenge.
const HeaderWWWAuthenticate = "WWW-Authenticate"

// challengeRealm is the protection space named in the challenge. Every MCP
// route of an application shares one realm: the credential that opens one opens
// all of them.
const challengeRealm = "mcp"

// Bearer error codes a challenge states (RFC 6750 3.1).
const (
	challengeInvalidToken      = "invalid_token"
	challengeInsufficientScope = "insufficient_scope"
)

// ChallengeValue builds the WWW-Authenticate header value for a 401 on a
// protected MCP route that was asked for without credentials.
// resourceMetadataURL is the protected-resource metadata document a client
// fetches to discover the authorization server (RFC 9728 5.1); scope is the
// scope it must request. Either may be empty and is then omitted, which leaves
// the realm alone for an application that publishes no discovery document and
// names no scope.
//
// It states no error. A request that carried no credentials has not failed at
// anything, and RFC 6750 3 has the server say so by leaving the error code out;
// InvalidTokenChallengeValue is the value for a request whose token was
// refused.
//
// Values are quoted-string auth-params, so any double quote or backslash in
// them is escaped rather than closing the string early.
func ChallengeValue(resourceMetadataURL, scope string) string {
	return challengeValue("", resourceMetadataURL, scope)
}

// InvalidTokenChallengeValue builds the WWW-Authenticate header value for a 401
// on a request whose bearer token was refused: expired, revoked, malformed, or
// issued for another resource. It is ChallengeValue with error="invalid_token"
// (RFC 6750 3.1), which is what tells a client that the token it holds is the
// problem and has to be replaced, rather than that it forgot to send one.
func InvalidTokenChallengeValue(resourceMetadataURL, scope string) string {
	return challengeValue(challengeInvalidToken, resourceMetadataURL, scope)
}

// InsufficientScopeChallengeValue builds the WWW-Authenticate header value for
// a 403 on a request whose token is valid and does not carry the scope the call
// needs (RFC 6750 3.1 error="insufficient_scope"). scope names what the call
// needs, which is what the MCP authorization specification has a client request
// when it steps its authorization up, and resourceMetadataURL lets it find the
// authorization server to do that with.
func InsufficientScopeChallengeValue(resourceMetadataURL, scope string) string {
	return challengeValue(challengeInsufficientScope, resourceMetadataURL, scope)
}

// InsufficientScope puts the insufficient_scope challenge on the response in c,
// for a guard that is about to refuse the request with a 403 because its token
// lacks scope. scope is what the call needs; empty means the scope the Config
// requires. The metadata URL is the one Challenge advertises for the same
// request. The guard then answers 403 however it answers anything else.
//
// Nothing attaches this challenge on its own. A 403 can mean things no new
// authorization would cure, and only the guard knows that this one is about
// scope, and which scope: told to step up for a scope it already holds, a
// client would send its user through consent again for nothing.
func InsufficientScope(c *router.Context, cfg Config, scope string) {
	if c == nil || c.Response == nil {
		return
	}
	if scope == "" {
		scope = cfg.scope()
	}
	c.Response.Header().Set(HeaderWWWAuthenticate, InsufficientScopeChallengeValue(cfg.challengeMetadataURL(c), scope))
}

// challengeValue assembles a bearer challenge: the realm, then the error code
// when there is one, the metadata URL and the scope.
func challengeValue(code, resourceMetadataURL, scope string) string {
	// Encoded first, then inspected: a value that is empty once the characters
	// a header cannot carry are gone has nothing to tell a client, and an empty
	// auth-param is worse than none.
	params := []struct{ name, value string }{
		{"error", quoteEscape(code)},
		{"resource_metadata", quoteEscape(resourceMetadataURL)},
		{"scope", quoteEscape(scope)},
	}

	var b strings.Builder
	b.WriteString(`Bearer realm="`)
	b.WriteString(quoteEscape(challengeRealm))
	b.WriteString(`"`)
	for _, param := range params {
		if param.value == "" {
			continue
		}
		b.WriteString(", ")
		b.WriteString(param.name)
		b.WriteString(`="`)
		b.WriteString(param.value)
		b.WriteString(`"`)
	}
	return b.String()
}

// presentsBearerToken reports whether a request carries a bearer token: an
// Authorization header of the Bearer scheme with something after it. That is
// what separates a request whose token was refused from one that came without
// credentials, or with credentials of a scheme this resource does not take,
// which RFC 6750 3 treats as having none.
func presentsBearerToken(r *http.Request) bool {
	if r == nil {
		return false
	}
	scheme, token, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	return found && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != ""
}

// quoteEscape escapes the two characters that are special inside an HTTP
// quoted-string (RFC 9110 5.6.4) and strips the control characters a
// quoted-string may not contain, so a hostile Host header or a misconfigured
// scope cannot inject extra auth-params or split the header.
func quoteEscape(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		ch := v[i]
		switch {
		case ch == '"' || ch == '\\':
			b.WriteByte('\\')
			b.WriteByte(ch)
		case ch < 0x20 || ch == 0x7f:
			// Control characters (CR and LF above all) are dropped rather than
			// escaped: they have no legal place in a header value.
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// Challenge returns middleware that attaches a bearer challenge to any 401 the
// rest of the chain produces for this route, so a client that is refused learns
// where to authorize instead of only that it was refused.
//
// A request that presented a bearer token and is answered 401 had that token
// refused, and its challenge says error="invalid_token" so the client replaces
// the token instead of sending it again. A request that presented none gets the
// same challenge without an error code, as RFC 6750 3 asks. A 403 is left to
// the guard: see InsufficientScope.
//
// It authenticates nothing itself. Install it outermost on an MCP route, ahead
// of whatever guard actually rejects the request; the guard may reject in
// either of the two ways a velocity handler can, and both are covered:
//
//   - by writing the response itself (c.Status(401), c.JSON(401, ...)), which
//     is intercepted as the status is committed, while the headers are still
//     mutable. The interception stays installed on the context for the rest of
//     the request, so a 401 that an error renderer on an enclosing group
//     (router.ErrorHandlerMiddleware) writes through the context after this
//     middleware has returned is covered too;
//   - by returning an error whose status is 401 (contract.NewHTTPError(401),
//     problem.Unauthorized(), velocity's auth errors, or any error wrapping
//     one), which the router's error boundary renders after every middleware
//     has returned and through its own response writer, past the context, so
//     the header is set on the way out instead. That covers the router's
//     default rendering, the application's error pipeline and a handler
//     installed with SetErrorHandler alike.
//
// The resource_metadata URL is derived per request from the path being served,
// so one instance mounted on several MCP routes advertises the right document
// for each: a 401 on /mcp/weather names
// /.well-known/oauth-protected-resource/mcp/weather. Config.ResourceMetadataURL
// overrides the derivation for a deployment whose document lives elsewhere.
//
// A WWW-Authenticate value the chain set itself is left alone: the guard knows
// which credential it rejected and may have named another protected resource,
// its own scope, or an error code this middleware cannot derive from the route.
// This challenge fills the gap RFC 9110 15.5.2 forbids, a 401 carrying no
// challenge at all, rather than overruling one that is already there.
func Challenge(cfg Config) router.MiddlewareFunc {
	scope := cfg.scope()

	return func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			// Settled before the chain runs: whether the request came with a
			// token is a fact about the request, and the chain is free to strip
			// the header once it has read it.
			value := ChallengeValue(cfg.challengeMetadataURL(c), scope)
			if presentsBearerToken(c.Request) {
				value = InvalidTokenChallengeValue(cfg.challengeMetadataURL(c), scope)
			}

			original := c.Response
			cw := &challengeWriter{ResponseWriter: original, challenge: value}
			// Deliberately not restored on the way out: the router resets the
			// context before returning it to its pool, and leaving the wrapper
			// in place is what lets an error renderer on an enclosing group,
			// writing through the context, still be intercepted. The router's
			// own error boundary puts its writer back before it renders, so
			// what it writes never passes through here.
			c.Response = cw

			err := next(c)

			// Nothing written through the context means whatever renders the
			// response next writes past this wrapper, so a pre-commit hook
			// registered on it would never fire. Run it here instead, which is
			// still ahead of the commit and the last point that is true.
			if !cw.committed {
				cw.fireBeforeFirstWrite()
			}

			// Nothing has been committed yet when the guard reports the refusal
			// as an error: the router's error boundary renders it later, through
			// its own response writer rather than the context. The header goes
			// on now, while it can still be changed. Should an error renderer on
			// an enclosing group answer through the context with something other
			// than a 401, the wrapper takes it back off; the boundary's own
			// answer never passes through the wrapper, so there the error's
			// status is the whole decision (see isUnauthorized). A guard that
			// set its own challenge before returning the error keeps it.
			if !cw.committed && isUnauthorized(err) && original.Header().Get(HeaderWWWAuthenticate) == "" {
				original.Header().Set(HeaderWWWAuthenticate, value)
				cw.speculative = true
			}
			return err
		}
	}
}

// challengeMetadataURL returns the protected-resource metadata URL to advertise
// for the resource this request is addressing, or "" when the application
// publishes no metadata.
func (cfg Config) challengeMetadataURL(c *router.Context) string {
	if cfg.ResourceMetadataURL != "" {
		return cfg.ResourceMetadataURL
	}
	if cfg.WithoutResourceMetadata {
		return ""
	}
	origin := cfg.origin(c.Request)
	if origin == "" {
		return ""
	}
	return metadataURLFor(origin, requestPath(c))
}

// isUnauthorized reports whether err is a handler-returned error the router's
// error boundary answers with a 401.
//
// The status is resolved the way the boundary resolves it: the first
// contract.StatusError in err's chain names it (contract.StatusOf), so an error
// wrapping a 401, or joined with one, is a 401 too. The boundary writes nothing
// for an error marking the response already written (contract.Handled), and
// answers a recovered panic with a 500 whatever its value carries, so neither
// counts.
//
// This path is only for errors the boundary renders past the context, which is
// all of them: it puts the router's own writer back before rendering. The
// decision rests on the status the error names, so an error handler that
// answers such an error with another status (a map rule, a render rule, a
// handler installed with SetErrorHandler) still carries the challenge, which
// RFC 9110 11.6.1 allows on any response; one that turns an error naming no
// 401 into a 401 carries none.
func isUnauthorized(err error) bool {
	if err == nil || contract.IsResponseWritten(err) {
		return false
	}
	var recovered contract.RecoveredPanic
	if errors.As(err, &recovered) {
		return false
	}
	status, _, named := contract.StatusOf(err)
	return named && status == http.StatusUnauthorized
}

// challengeWriter wraps the response writer for the duration of one MCP request
// so a 401 written by the chain gains the challenge header at the instant the
// status is committed, which is the last moment headers can still be changed.
// Any other status passes through untouched: a challenge on a successful
// response would tell a client to re-authorize a call that just worked.
//
// It forwards the optional writer interfaces velocity's own wrapper forwards
// (Flusher for the streamed event-stream transport, Hijacker, Pusher, and the
// BeforeFirstWrite pre-commit hook) plus Unwrap for http.ResponseController, so
// wrapping the writer costs the handler no capability.
type challengeWriter struct {
	http.ResponseWriter
	challenge string
	committed bool

	// beforeFirstWrite is the pre-commit hook registered through
	// BeforeFirstWrite, cleared as it fires so it runs at most once.
	beforeFirstWrite func()

	// speculative records that the middleware already put the challenge on the
	// response in anticipation of a 401 the router had not rendered yet. If the
	// status that finally arrives through this writer is a different one, the
	// header is withdrawn again so it never contradicts the response.
	speculative bool
}

// BeforeFirstWrite registers fn to run once, immediately before this response
// commits its status line. Velocity's save-at-end session middleware finds the
// hook by asserting this method on the response writer, so a middleware
// installed inside Challenge would otherwise lose its pre-commit save and write
// its Set-Cookie into headers that have already gone out.
//
// The hook is held here rather than passed down to the wrapped writer: the
// framework's writer keeps a single hook, so forwarding would overwrite one a
// middleware outside Challenge had already registered on it. Both fire, this
// one first, because the wrapped writer's own hook runs as it commits.
func (w *challengeWriter) BeforeFirstWrite(fn func()) {
	if fn == nil {
		return
	}
	w.beforeFirstWrite = fn
}

// fireBeforeFirstWrite runs the registered hook at most once. Clearing the
// field before the call is what makes it once, and keeps a hook that writes
// through this writer from recursing.
func (w *challengeWriter) fireBeforeFirstWrite() {
	fn := w.beforeFirstWrite
	w.beforeFirstWrite = nil
	if fn != nil {
		fn()
	}
}

// WriteHeader sets the challenge header on a 401 that carries none of its own
// before the status is written, then commits it. Repeated calls are ignored,
// matching net/http.
func (w *challengeWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.fireBeforeFirstWrite()
	w.committed = true
	header := w.ResponseWriter.Header()
	switch {
	case status == http.StatusUnauthorized && w.challenge != "":
		// Whatever the chain already advertises is what a client acts on: it
		// may name an authorization server, a scope or an error this route
		// cannot know about.
		if header.Get(HeaderWWWAuthenticate) == "" {
			header.Set(HeaderWWWAuthenticate, w.challenge)
		}
	case w.speculative && header.Get(HeaderWWWAuthenticate) == w.challenge:
		// Only the anticipated challenge is withdrawn, never a value the
		// renderer put there on its way to a different status.
		header.Del(HeaderWWWAuthenticate)
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write commits a 200 first when the handler writes a body without setting a
// status, mirroring net/http so committed tracks reality.
func (w *challengeWriter) Write(b []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush forwards to the wrapped writer when it streams, marking the response
// committed because the first flush writes the status line. The pre-commit hook
// fires first, for the same reason.
func (w *challengeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.committed {
			w.fireBeforeFirstWrite()
		}
		w.committed = true
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer when it supports connection takeover,
// running the pre-commit hook first so anything it adds to the headers reaches
// the connection before the hijacker writes its own response.
func (w *challengeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		if !w.committed {
			w.fireBeforeFirstWrite()
		}
		w.committed = true
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Push forwards an HTTP/2 server push to the wrapped writer when supported. It
// does not commit the main response: a push opens its own stream, so the
// pre-commit hook stays armed for the response the client asked for.
func (w *challengeWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
