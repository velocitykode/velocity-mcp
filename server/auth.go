package server

import "context"

// Identity is the authenticated principal for the current MCP call. Its method
// set is velocity's authenticated-user contract, so whatever the application's
// auth scheme resolved (its own user model, an auth.AuthUser, ...) satisfies it
// as it stands and a handler can assert back to the concrete type.
//
// It is declared here rather than aliased so this package stays free of the
// HTTP and auth stacks: a server served only over stdio links neither.
type Identity interface {
	GetAuthIdentifier() any
	GetAuthPassword() string
	GetRememberToken() string
	SetRememberToken(token string)
}

// IdentityResolver answers who is calling under a named authentication scheme,
// or nil when that scheme authenticates nobody for this call. An empty name
// means the application's default scheme.
//
// A transport that carries an authenticated request installs one on the request
// context; a transport with no request behind it installs none, and Request.User
// then answers nil.
type IdentityResolver func(scheme string) Identity

// identityResolverKey keys the resolver on the inbound request context. A
// struct{} key is unexported and type-unique, so nothing outside this package
// can collide with it or overwrite the value.
type identityResolverKey struct{}

// WithIdentityResolver returns ctx carrying resolve, which Request.User calls to
// answer who is making the current call. A nil resolve returns ctx unchanged, so
// a transport may call this unconditionally.
//
// The resolver must be bound to values that outlive the call it describes: a
// velocity router context is pooled and recycled the moment the HTTP handler
// returns, so a resolver that closed over one would report another request's
// identity to a handler that outlived its own.
func WithIdentityResolver(ctx context.Context, resolve IdentityResolver) context.Context {
	if resolve == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, identityResolverKey{}, resolve)
}

// IdentityResolverFrom returns the resolver carried by ctx, or nil when the call
// did not arrive over an authenticating transport.
func IdentityResolverFrom(ctx context.Context) IdentityResolver {
	if ctx == nil {
		return nil
	}
	resolve, _ := ctx.Value(identityResolverKey{}).(IdentityResolver)
	return resolve
}

// User returns the identity authenticated for this call, or nil when the call is
// unauthenticated, arrived over a transport with no request behind it (stdio,
// the fake transport), or the application has no auth configured.
//
// With no argument the identity is resolved through the application's default
// auth scheme. An MCP endpoint is usually guarded by a scheme of its own (a
// bearer or JWT scheme registered alongside the browser session scheme an
// application defaults to), and naming it resolves against that one instead:
//
//	user := req.User("api")
//
// Only the first name is used; the rest are ignored. An unknown scheme resolves
// nobody rather than failing the call. This package neither authenticates nor
// authorizes: whatever middleware guards the route decides who the caller is,
// and handlers must treat a nil result as "not authenticated" rather than as a
// transport failure.
func (r *Request) User(scheme ...string) Identity {
	resolve := IdentityResolverFrom(r.ctx)
	if resolve == nil {
		return nil
	}
	name := ""
	if len(scheme) > 0 {
		name = scheme[0]
	}
	return resolve(name)
}
