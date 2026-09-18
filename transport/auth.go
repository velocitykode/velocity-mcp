package transport

import (
	"github.com/velocitykode/velocity/auth"
	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/server"
)

// identityResolver builds the resolver server.Request.User answers from, or nil
// when this request has no authentication behind it (no service container, no
// auth manager, or a manager that is not velocity's own).
//
// Both values it closes over are stable for longer than the call that is being
// described: the auth manager belongs to the application, and the *http.Request
// belongs to the connection. The velocity router context itself is deliberately
// not captured, because the router resets and pools it the moment the handler
// returns, and a handler that hands its request context to a goroutine would
// otherwise read whichever request landed on that context next.
func identityResolver(c *router.Context) server.IdentityResolver {
	if c == nil || c.Request == nil {
		return nil
	}
	manager := auth.FromContext(c)
	if manager == nil {
		return nil
	}
	req := c.Request

	return func(name string) server.Identity {
		scheme, err := manager.Scheme(name)
		if err != nil || scheme == nil {
			return nil
		}
		user := scheme.User(req)
		if user == nil {
			return nil
		}
		return user
	}
}
