package client

import "github.com/velocitykode/velocity-mcp/client/oauth"

// WebClient is a Client over the HTTP transport with bearer-token and OAuth
// helpers. It embeds *Client, so all the standard operations (Tools, CallTool,
// …) are available directly.
type WebClient struct {
	*Client

	transport   *HTTPTransport
	oauthConfig *oauth.Config
}

// WithToken sets a static bearer token sent on every request.
//
// The token is the authorization context of everything fetched with it. The
// specification forbids reusing a result scoped to one context in another, and
// a different access token is a different context, so nothing the client keeps
// outlives a change of token: the connection is negotiated again under the new
// one by the next request, the session the old one opened is released with the
// old one, and what the server advertised, the tool definitions it stated, and
// the tools the client refused are all read again rather than carried across.
func (w *WebClient) WithToken(token string) *WebClient {
	w.transport.WithToken(token)
	return w
}

// WithTokenFunc sets a callback resolving the bearer token. It is asked once
// for each request the client makes, and every frame of that request presents
// the token it gave. A token that differs from the one before is another
// authorization context, exactly as one set through WithToken is, whether the
// callback rotates the token of one caller or answers for a different one.
func (w *WebClient) WithTokenFunc(fn func() string) *WebClient {
	w.transport.WithTokenFunc(fn)
	return w
}

// WithOAuth records the OAuth client configuration used by OAuthClient.
func (w *WebClient) WithOAuth(config oauth.Config) *WebClient {
	w.oauthConfig = &config
	return w
}

// OAuthClient builds an OAuth client for this server's URL using the
// configuration set by WithOAuth. resourceMetadataURL and challengeScope are
// typically taken from an oauth.AuthorizationRequiredError raised by a prior
// request. It fails if WithOAuth has not been called.
func (w *WebClient) OAuthClient(resourceMetadataURL, challengeScope string) (*oauth.Client, error) {
	if w.oauthConfig == nil {
		return nil, newError("no OAuth configuration found; call WithOAuth before OAuthClient")
	}
	return oauth.NewClient(*w.oauthConfig, w.transport.URL(), resourceMetadataURL, challengeScope), nil
}
