package mcpclient

import (
	"fmt"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/client"
)

// For returns a WebClient for a registered MCP server with the current
// browser's stored bearer token attached (if any). Use it in handlers after the
// OAuth flow has completed to make authorized calls:
//
//	w, err := mcpclient.For(c, "example")
//	tools, err := w.Tools(c.Request.Context())
func For(c *router.Context, name string) (*client.WebClient, error) {
	e, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("mcpclient: client [%s] is not registered", name)
	}
	w := client.Web(e.resourceURL)
	if token, _ := defaultStore.Token(c, name); token != "" {
		w.WithToken(token)
	}
	return w, nil
}

// Token returns the stored access token for a registered client and whether one
// is present for the current browser.
func Token(c *router.Context, name string) (string, bool) {
	token, err := defaultStore.Token(c, name)
	if err != nil || token == "" {
		return "", false
	}
	return token, true
}

// Authorized reports whether the current browser holds a token for the client.
func Authorized(c *router.Context, name string) bool {
	_, ok := Token(c, name)
	return ok
}
