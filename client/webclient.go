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
func (w *WebClient) WithToken(token string) *WebClient {
	w.transport.WithToken(token)
	return w
}

// WithTokenFunc sets a callback resolving the bearer token per request.
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
