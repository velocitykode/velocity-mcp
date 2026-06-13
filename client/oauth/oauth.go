package oauth

import (
	"context"
	"crypto/subtle"
	"net/url"
	"time"

	"github.com/velocitykode/velocity/httpclient"
	"github.com/velocitykode/velocity/str"
)

// DefaultScope is requested when neither the config nor the challenge specify a
// scope.
const DefaultScope = "mcp:use"

// stateLength is the byte length of the random anti-CSRF state parameter.
const stateLength = 40

// Client drives the OAuth flows for a single protected MCP resource. It is
// constructed from a Config plus the resource URL and (optionally) the
// challenge-advertised metadata URL and scope. Discovery is performed lazily and
// memoized on first use.
type Client struct {
	config              Config
	resourceURL         string
	resourceMetadataURL string
	challengeScope      string

	discovery  *Discovery
	httpClient *httpclient.Client
	now        func() time.Time

	discovered *DiscoveryResult
}

// NewClient builds an OAuth client for resourceURL. resourceMetadataURL and
// challengeScope are typically taken from a server's WWW-Authenticate challenge
// and may be empty.
func NewClient(config Config, resourceURL, resourceMetadataURL, challengeScope string) *Client {
	return &Client{
		config:              config,
		resourceURL:         beforeFragment(resourceURL),
		resourceMetadataURL: resourceMetadataURL,
		challengeScope:      challengeScope,
		discovery:           NewDiscovery(),
		httpClient:          endpointClient(),
		now:                 time.Now,
	}
}

// PendingAuthorization is the per-attempt state a caller must persist between
// AuthorizationURL and ExchangeCode. It binds the redirect to its PKCE verifier,
// anti-CSRF state, resolved client credentials, and expected issuer. Persist it
// server-side keyed to the user's session; never expose it to the browser.
type PendingAuthorization struct {
	State           string
	Verifier        string
	ClientID        string
	ClientSecret    string
	TokenEndpoint   string
	TokenAuthMethod string
	RedirectURI     string
	ReturnTo        string
	Issuer          string
	IssuerSupported bool
}

// AuthorizationURL performs discovery (and, if no client_id is configured,
// dynamic client registration), then returns the authorization endpoint URL to
// redirect the user to along with the PendingAuthorization the caller must
// persist for ExchangeCode. returnTo is an opaque value echoed back from
// ExchangeCode after a successful exchange.
func (c *Client) AuthorizationURL(ctx context.Context, returnTo string) (string, *PendingAuthorization, error) {
	discovered, err := c.discover(ctx)
	if err != nil {
		return "", nil, err
	}
	metadata := discovered.Server

	if len(metadata.CodeChallengeMethodsSupported) > 0 && !contains(metadata.CodeChallengeMethodsSupported, "S256") {
		return "", nil, newError("the authorization server does not support the required S256 PKCE method")
	}

	if c.config.RedirectURI == "" {
		return "", nil, newError("a redirect URI is required")
	}
	redirectURI := c.config.RedirectURI

	clientID := c.config.ClientID
	clientSecret := c.config.ClientSecret
	if clientID == "" {
		reg, err := c.register(ctx, metadata, redirectURI)
		if err != nil {
			return "", nil, err
		}
		clientID = reg.ClientID
		clientSecret = reg.ClientSecret
	}

	pkce, err := GeneratePKCE()
	if err != nil {
		return "", nil, err
	}
	state, err := str.Random(stateLength)
	if err != nil {
		return "", nil, wrapError(err, "unable to generate OAuth state")
	}

	pending := &PendingAuthorization{
		State:           state,
		Verifier:        pkce.Verifier,
		ClientID:        clientID,
		ClientSecret:    clientSecret,
		TokenEndpoint:   metadata.TokenEndpoint,
		TokenAuthMethod: resolveTokenAuthMethod(metadata, clientSecret),
		RedirectURI:     redirectURI,
		ReturnTo:        returnTo,
		Issuer:          metadata.Issuer,
		IssuerSupported: metadata.AuthorizationResponseIssParameterSupported,
	}

	authURL, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return "", nil, wrapError(err, "unable to parse authorization endpoint [%s]", metadata.AuthorizationEndpoint)
	}
	q := authURL.Query()
	setIfPresent(q, "response_type", "code")
	setIfPresent(q, "client_id", clientID)
	setIfPresent(q, "redirect_uri", redirectURI)
	setIfPresent(q, "state", state)
	setIfPresent(q, "code_challenge", pkce.Challenge)
	setIfPresent(q, "code_challenge_method", "S256")
	setIfPresent(q, "scope", c.resolveScope())
	setIfPresent(q, "resource", c.resourceURL)
	authURL.RawQuery = q.Encode()

	return authURL.String(), pending, nil
}

// ExchangeCode completes the authorization-code grant. callbackQuery is the
// query string of the provider's redirect to the callback URL. It validates any
// error parameter, the anti-CSRF state, and the issuer (iss) parameter against
// the persisted PendingAuthorization before exchanging the code for tokens. The
// returned returnTo echoes the value passed to AuthorizationURL.
func (c *Client) ExchangeCode(ctx context.Context, pending *PendingAuthorization, callbackQuery url.Values) (token *TokenSet, returnTo string, err error) {
	if pending == nil {
		return nil, "", newError("no pending OAuth authorization was provided")
	}
	if oauthErr := callbackQuery.Get("error"); oauthErr != "" {
		if desc := callbackQuery.Get("error_description"); desc != "" {
			return nil, "", newError("the authorization server returned an error [%s]: %s", oauthErr, desc)
		}
		return nil, "", newError("the authorization server returned an error [%s]", oauthErr)
	}

	code := callbackQuery.Get("code")
	if code == "" {
		return nil, "", newError("the OAuth callback did not include an authorization code")
	}

	if subtle.ConstantTimeCompare([]byte(pending.State), []byte(callbackQuery.Get("state"))) != 1 {
		return nil, "", newError("the OAuth state parameter did not match; possible CSRF attempt")
	}
	if err := validateIssuer(pending, callbackQuery.Get("iss")); err != nil {
		return nil, "", err
	}

	authMethod := pending.TokenAuthMethod
	if authMethod == "" {
		authMethod = authMethodPost
	}

	tok, err := c.requestToken(ctx, pending.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {pending.RedirectURI},
		"code_verifier": {pending.Verifier},
		"resource":      {c.resourceURL},
	}, pending.ClientID, pending.ClientSecret, authMethod)
	if err != nil {
		return nil, "", err
	}
	tok.ClientID = pending.ClientID
	tok.ClientSecret = pending.ClientSecret
	return tok, pending.ReturnTo, nil
}

// ClientCredentials performs the client-credentials grant. A client_id is
// required.
func (c *Client) ClientCredentials(ctx context.Context) (*TokenSet, error) {
	if c.config.ClientID == "" {
		return nil, newError("a client_id is required for the client_credentials grant")
	}
	discovered, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"client_credentials"}, "resource": {c.resourceURL}}
	if scope := c.resolveScope(); scope != "" {
		form.Set("scope", scope)
	}
	return c.requestToken(ctx, discovered.Server.TokenEndpoint, form,
		c.config.ClientID, c.config.ClientSecret,
		resolveTokenAuthMethod(discovered.Server, c.config.ClientSecret))
}

// Refresh exchanges a refresh token for a fresh token set. clientID and
// clientSecret override the configured credentials when non-empty (a refresh
// typically reuses the credentials stored on the previous TokenSet).
func (c *Client) Refresh(ctx context.Context, refreshToken, clientID, clientSecret string) (*TokenSet, error) {
	discovered, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	if clientID == "" {
		clientID = c.config.ClientID
	}
	if clientSecret == "" {
		clientSecret = c.config.ClientSecret
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"resource":      {c.resourceURL},
	}
	if scope := c.resolveScope(); scope != "" {
		form.Set("scope", scope)
	}
	tok, err := c.requestToken(ctx, discovered.Server.TokenEndpoint, form,
		clientID, clientSecret, resolveTokenAuthMethod(discovered.Server, clientSecret))
	if err != nil {
		return nil, err
	}
	tok.ClientID = clientID
	tok.ClientSecret = clientSecret
	return tok, nil
}

// requestToken posts a token request, applying the chosen client-authentication
// method, and parses the resulting TokenSet.
func (c *Client) requestToken(ctx context.Context, tokenEndpoint string, form url.Values, clientID, clientSecret, authMethod string) (*TokenSet, error) {
	var basicAuth *[2]string
	switch authMethod {
	case authMethodBasic:
		basicAuth = &[2]string{clientID, clientSecret}
	case authMethodNone:
		form.Set("client_id", clientID)
	default: // authMethodPost
		form.Set("client_id", clientID)
		if clientSecret != "" {
			form.Set("client_secret", clientSecret)
		}
	}

	status, data, err := postForm(ctx, c.httpClient, tokenEndpoint, form, basicAuth)
	if err != nil {
		return nil, err
	}
	if !successful(status) {
		return nil, newError("token request to [%s] failed with status [%d]", tokenEndpoint, status)
	}
	if stringField(data, "access_token") == "" {
		return nil, newError("the token response did not include an access_token")
	}
	tok := tokenSetFromResponse(data, c.now())
	return &tok, nil
}

// register performs dynamic client registration when no client_id is configured.
func (c *Client) register(ctx context.Context, metadata *AuthServerMetadata, redirectURI string) (*ClientRegistration, error) {
	if metadata.RegistrationEndpoint == "" {
		return nil, newError("no client_id was configured and the authorization server does not support dynamic client registration")
	}
	return registerClient(ctx, c.httpClient, metadata.RegistrationEndpoint, redirectURI,
		c.resolveScope(), applicationType(redirectURI), resolveTokenAuthMethod(metadata, "confidential"))
}

// discover resolves and memoizes the authorization-server metadata.
func (c *Client) discover(ctx context.Context) (*DiscoveryResult, error) {
	if c.discovered != nil {
		return c.discovered, nil
	}
	result, err := c.discovery.Discover(ctx, c.resourceURL, c.resourceMetadataURL)
	if err != nil {
		return nil, err
	}
	c.discovered = result
	return result, nil
}

// resolveScope picks the effective scope: the challenge scope, then the
// configured scope, then the default.
func (c *Client) resolveScope() string {
	if c.challengeScope != "" {
		return c.challengeScope
	}
	if c.config.Scope != "" {
		return c.config.Scope
	}
	return DefaultScope
}

// validateIssuer enforces RFC 9207 issuer binding: when the server returns iss
// it must match the expected issuer; when the server supports iss but omits it,
// the response is rejected.
func validateIssuer(pending *PendingAuthorization, iss string) error {
	if iss != "" {
		if pending.Issuer == "" || subtle.ConstantTimeCompare([]byte(pending.Issuer), []byte(iss)) != 1 {
			return newError("the OAuth issuer (iss) parameter did not match the expected issuer; possible mix-up attack")
		}
		return nil
	}
	if pending.IssuerSupported {
		return newError("the authorization response is missing the required iss parameter")
	}
	return nil
}

// resolveTokenAuthMethod chooses the client-authentication method: none for a
// public client (no secret), client_secret_basic when the server supports basic
// but not post, otherwise client_secret_post.
func resolveTokenAuthMethod(metadata *AuthServerMetadata, clientSecret string) string {
	if clientSecret == "" {
		return authMethodNone
	}
	supported := metadata.TokenEndpointAuthMethodsSupported
	if len(supported) > 0 && !contains(supported, authMethodPost) && contains(supported, authMethodBasic) {
		return authMethodBasic
	}
	return authMethodPost
}

// applicationType classifies a redirect URI as native (loopback host) or web.
func applicationType(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "web"
	}
	if isLocalhost(normalizedHost(u.Hostname())) {
		return "native"
	}
	return "web"
}

// setIfPresent sets a query parameter only when value is non-empty.
func setIfPresent(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

// contains reports whether s is present in values.
func contains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}
