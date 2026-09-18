package server

import (
	"context"
	"sync"
	"testing"

	"github.com/velocitykode/velocity/auth"
)

// person is the shape an application's user model takes: it satisfies the
// identity contract without this package knowing anything about it.
type person struct {
	id   uint
	name string
}

func (p *person) GetAuthIdentifier() any        { return p.id }
func (p *person) GetAuthPassword() string       { return "" }
func (p *person) GetRememberToken() string      { return "" }
func (p *person) SetRememberToken(token string) {}

// schemes builds a resolver over a fixed set of named schemes, the shape a
// transport installs: "" addresses defaultScheme, an unknown name nobody.
func schemes(defaultScheme string, users map[string]Identity) IdentityResolver {
	return func(name string) Identity {
		if name == "" {
			name = defaultScheme
		}
		user, ok := users[name]
		if !ok {
			return nil
		}
		return user
	}
}

func TestRequestUser(t *testing.T) {
	ada := &person{id: 7, name: "Ada"}
	grace := &person{id: 9, name: "Grace"}

	tests := []struct {
		name   string
		ctx    func() context.Context
		scheme []string
		want   Identity
	}{
		{
			name: "a request built without a context has no identity",
			ctx:  func() context.Context { return nil },
		},
		{
			name: "a transport with no request behind it installs no resolver",
			ctx:  func() context.Context { return context.Background() },
		},
		{
			name: "the default scheme answers an unnamed call",
			ctx: func() context.Context {
				return WithIdentityResolver(context.Background(), schemes("web", map[string]Identity{"web": ada}))
			},
			want: ada,
		},
		{
			name: "the default scheme authenticates nobody",
			ctx: func() context.Context {
				return WithIdentityResolver(context.Background(), schemes("web", map[string]Identity{}))
			},
		},
		{
			name: "a named scheme is resolved instead of the default",
			ctx: func() context.Context {
				return WithIdentityResolver(context.Background(),
					schemes("web", map[string]Identity{"web": ada, "api": grace}))
			},
			scheme: []string{"api"},
			want:   grace,
		},
		{
			name: "an unknown scheme resolves nobody rather than falling back",
			ctx: func() context.Context {
				return WithIdentityResolver(context.Background(),
					schemes("web", map[string]Identity{"web": ada}))
			},
			scheme: []string{"nope"},
		},
		{
			name: "an empty name is the default scheme",
			ctx: func() context.Context {
				return WithIdentityResolver(context.Background(),
					schemes("web", map[string]Identity{"web": ada, "api": grace}))
			},
			scheme: []string{""},
			want:   ada,
		},
		{
			name: "only the first name is consulted",
			ctx: func() context.Context {
				return WithIdentityResolver(context.Background(),
					schemes("web", map[string]Identity{"web": ada, "api": grace}))
			},
			scheme: []string{"api", "web"},
			want:   grace,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := NewRequest(map[string]any{"city": "Berlin"})
			if ctx := tt.ctx(); ctx != nil {
				req = req.WithRequestContext(ctx)
			}

			got := req.User(tt.scheme...)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("User(%v) = %#v, want nil", tt.scheme, got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("User(%v) = %#v, want %#v", tt.scheme, got, tt.want)
			}
			if id := got.GetAuthIdentifier(); id != tt.want.GetAuthIdentifier() {
				t.Fatalf("identifier = %#v, want %#v", id, tt.want.GetAuthIdentifier())
			}
		})
	}
}

// A nil context is what a caller that forgot to thread one hands in, so both
// directions have to survive it. The nil is held in a variable rather than
// written as a literal so the call sites read as the accident they model.
func TestIdentityResolver_NilSafety(t *testing.T) {
	var missing context.Context
	resolve := schemes("web", map[string]Identity{"web": &person{id: 1}})

	if got := WithIdentityResolver(context.Background(), nil); got != context.Background() {
		t.Fatal("storing a nil resolver changed the context")
	}
	if got := WithIdentityResolver(missing, resolve); IdentityResolverFrom(got) == nil {
		t.Fatal("a nil parent context lost the resolver")
	}
	if got := IdentityResolverFrom(missing); got != nil {
		t.Fatal("IdentityResolverFrom(nil) returned a resolver")
	}
	if got := IdentityResolverFrom(context.Background()); got != nil {
		t.Fatal("IdentityResolverFrom on a bare context returned a resolver")
	}
}

// WithRequestContext must never leave the request holding a nil context, or
// User would dereference it.
func TestRequestWithRequestContext_NilIsUsable(t *testing.T) {
	var missing context.Context
	req := NewRequest(nil).WithRequestContext(missing)

	if req.User() != nil {
		t.Fatal("User() on a request with no resolver must be nil")
	}
	// And the request still works for everything else it carries.
	if req.Has("nothing") {
		t.Fatal("a request built with no arguments reported one")
	}
}

// A typed nil identity is not "authenticated": a scheme that hands one back has
// resolved nobody, and a handler comparing the result to nil must agree.
func TestRequestUser_TypedNilIsNotAnIdentity(t *testing.T) {
	ctx := WithIdentityResolver(context.Background(), func(string) Identity {
		var absent *person
		if absent != nil {
			return absent
		}
		return nil
	})

	if got := NewRequest(nil).WithRequestContext(ctx).User(); got != nil {
		t.Fatalf("User() = %#v, want nil", got)
	}
}

// Primitive handlers for one session can run concurrently, and each reads the
// identity off its own request.
func TestRequestUser_Concurrent(t *testing.T) {
	ada := &person{id: 7, name: "Ada"}
	grace := &person{id: 9, name: "Grace"}
	resolve := schemes("web", map[string]Identity{"web": ada, "api": grace})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithIdentityResolver(context.Background(), resolve)
			req := NewRequest(nil).WithRequestContext(ctx)

			want, scheme := Identity(ada), []string(nil)
			if i%2 == 0 {
				want, scheme = grace, []string{"api"}
			}
			if got := req.User(scheme...); got != want {
				t.Errorf("User(%v) = %#v, want %#v", scheme, got, want)
			}
		}()
	}
	wg.Wait()
}

// Identity is declared locally so this package does not link the auth stack,
// but it must stay exactly velocity's authenticated-user contract: a value from
// the application's auth scheme has to satisfy it with no adapter, and a
// handler that asserts back to the concrete model has to keep working.
var _ Identity = auth.Authenticatable(nil)
var _ auth.Authenticatable = Identity(nil)
var _ Identity = (*auth.AuthUser)(nil)
