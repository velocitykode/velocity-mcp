package oauth

import (
	"context"
	"crypto/subtle"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/velocitykode/velocity/httpclient"
	"github.com/velocitykode/velocity/str"
)

// DefaultScope is the scope an MCP server built on this module advertises unless
// it is configured otherwise. A flow does not request it on its own account: it
// asks for what the server's challenge or metadata names (see Config.Scope), so
// a consumer that wants this scope regardless sets it as Config.Scope.
const DefaultScope = "mcp:use"

// stateLength is the byte length of the random anti-CSRF state parameter.
const stateLength = 40

// discoveryTTL bounds how long a Client reuses the authorization-server
// metadata it discovered. A long-lived Client is expected to notice an
// authorization server that moves an endpoint or starts advertising client ID
// metadata document support without the process being restarted.
const discoveryTTL = 15 * time.Minute

// registrationTTL bounds how long a Client reuses the client record it
// registered dynamically. Reuse is what keeps a burst of authorizations from
// creating one record per browser, but the authorization server decides how long
// a record lives: RFC 7591 gives no way to learn that a record has been dropped,
// and a client_secret_expires_at of 0 only promises the secret never expires,
// not that the registration survives. Without a ceiling, a server that forgets
// the record leaves every later flow presenting a client_id it no longer knows
// for the life of the process, with no error the application can act on.
// Registering again after registrationTTL bounds that outage while still
// collapsing per-flow churn.
const registrationTTL = time.Hour

// Client drives the OAuth flows for a single protected MCP resource. It is
// constructed from a Config plus the resource URL and (optionally) the
// challenge-advertised metadata URL and scope. Discovery and dynamic client
// registration are performed lazily and memoized, so a Client that is reused
// starts every flow against the same client record instead of creating a new
// one on the authorization server per flow. Discovered metadata is refreshed
// after discoveryTTL. An issued registration is discarded once its secret
// expires, once registrationTTL has elapsed, and as soon as discovery reports a
// different issuer or registration endpoint, since a client record is only ever
// accepted by the server that issued it. A Client is safe for concurrent use.
//
// Credentials are bound to the authorization server they belong to, so a
// protected resource that starts advertising a different one cannot collect
// them: an issued TokenSet records its issuer and is never refreshed against
// another server, and the configured client id and secret are only ever
// presented to Config.Issuer. Both refusals wrap ErrIssuerMismatch.
//
// The memoized registration belongs to the redirect URI this Client was
// configured with: an application serving several origins builds one Client per
// origin, never one shared Client, because credentials issued for one
// redirect_uri are rejected when presented with another.
type Client struct {
	config              Config
	resourceURL         string
	resourceMetadataURL string
	challengeScope      string

	discovery  *Discovery
	httpClient *httpclient.Client
	now        func() time.Time

	// gate serialises the network work behind discovery and registration, so a
	// burst of first flows performs each of them once rather than once per flow.
	// It is a channel rather than a mutex because a flow whose context ends
	// while it waits has to be able to give up: a browser that went away must
	// not keep a goroutine parked behind an authorization server that is slow to
	// answer somebody else.
	gate chan struct{}

	// mu guards the memoized discovery and registration results. It is only ever
	// held for field access, never across a request.
	mu             sync.Mutex
	discovered     *DiscoveryResult
	discoveredAt   time.Time
	registration   *ClientRegistration
	registeredAt   time.Time
	registeredWith string
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
		discovery:           discoveryFor(config),
		httpClient:          endpointClient(postureFor(config.AllowPrivateHosts, resourceURL)),
		now:                 time.Now,
		gate:                make(chan struct{}, 1),
	}
}

// discoveryFor picks the Discovery variant matching the config's host posture.
func discoveryFor(config Config) *Discovery {
	if config.AllowPrivateHosts {
		return NewDiscoveryAllowingPrivateHosts()
	}
	return NewDiscovery()
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
// resolves one from the client ID metadata document or dynamic client
// registration), then returns the authorization endpoint URL to redirect the
// user to along with the PendingAuthorization the caller must persist for
// ExchangeCode. returnTo is an opaque value echoed back from ExchangeCode after
// a successful exchange.
//
// The flow refuses to start unless the authorization server advertises PKCE
// with S256: a server that omits code_challenge_methods_supported gives no
// guarantee that the code challenge is enforced, so the returned error wraps
// ErrPKCERequired.
//
// It also refuses to commit the configured client credentials to a pending
// authorization for an authorization server other than Config.Issuer, so a
// resource that has started advertising a different one never gets a flow
// pointed at it. That error wraps ErrIssuerMismatch, and ErrIssuerRequired when
// the configuration holds credentials and names no issuer for them.
func (c *Client) AuthorizationURL(ctx context.Context, returnTo string) (string, *PendingAuthorization, error) {
	discovered, err := c.discover(ctx)
	if err != nil {
		return "", nil, err
	}
	metadata := discovered.Server

	if len(metadata.CodeChallengeMethodsSupported) == 0 {
		return "", nil, wrapError(ErrPKCERequired, "the authorization server metadata does not advertise [code_challenge_methods_supported]")
	}
	if !contains(metadata.CodeChallengeMethodsSupported, "S256") {
		return "", nil, wrapError(ErrPKCERequired, "the authorization server does not advertise the S256 code challenge method")
	}

	if c.config.RedirectURI == "" {
		return "", nil, newError("a redirect URI is required")
	}
	redirectURI := c.config.RedirectURI

	clientID := c.config.ClientID
	clientSecret := c.config.ClientSecret
	if clientID == "" {
		reg, err := c.resolveRegistration(ctx, discovered, redirectURI)
		if err != nil {
			return "", nil, err
		}
		clientID = reg.ClientID
		clientSecret = reg.ClientSecret
	}
	if err := c.requireConfiguredCredentialsBelongTo(metadata.Issuer, clientID, clientSecret); err != nil {
		return "", nil, err
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
	setIfPresent(q, "scope", c.resolveScope(discovered))
	setIfPresent(q, "resource", c.resourceURL)
	authURL.RawQuery = q.Encode()

	return authURL.String(), pending, nil
}

// ExchangeCode completes the authorization-code grant. callbackQuery is the
// query string of the provider's redirect to the callback URL. It validates the
// anti-CSRF state and the issuer (iss) parameter against the persisted
// PendingAuthorization before it reads anything else the response carries, then
// reports an error response or exchanges the code for tokens. The returned
// returnTo echoes the value passed to AuthorizationURL.
//
// The order is what RFC 9207 2.4 and the MCP authorization specification
// require: a response that does not answer this authorization request, or that
// does not come from the authorization server the request was sent to, is not
// acted on at all. That covers its error, error_description and error_uri as
// much as its code, because whoever forged the response chose that text, and a
// caller that shows the returned error to the user would be showing theirs.
//
// The exchange targets the authorization server the PendingAuthorization names,
// which was recorded when the flow started rather than discovered again here. A
// pending authorization naming an authorization server other than Config.Issuer
// is refused with an error wrapping ErrIssuerMismatch, so a flow started for a
// server the configuration does not name carries no credentials to it. The
// refusal reads the issuer the pending authorization records and nothing else,
// which is one more reason to persist that value server-side keyed to the
// session: a caller that lets it be rewritten has handed over the endpoints it
// records along with it.
func (c *Client) ExchangeCode(ctx context.Context, pending *PendingAuthorization, callbackQuery url.Values) (token *TokenSet, returnTo string, err error) {
	if pending == nil {
		return nil, "", newError("no pending OAuth authorization was provided")
	}
	if err := validateState(pending, callbackQuery.Get("state")); err != nil {
		return nil, "", err
	}
	if err := validateIssuer(pending, callbackQuery.Get("iss")); err != nil {
		return nil, "", err
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
	if err := c.requireConfiguredIssuer(pending.Issuer); err != nil {
		return nil, "", err
	}

	authMethod := pending.TokenAuthMethod
	if authMethod == "" {
		authMethod = authMethodPost
	}

	tok, err := c.requestToken(ctx, pending.Issuer, pending.TokenEndpoint, url.Values{
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
	if scope := c.resolveScope(discovered); scope != "" {
		form.Set("scope", scope)
	}
	return c.requestToken(ctx, discovered.Server.Issuer, discovered.Server.TokenEndpoint, form,
		c.config.ClientID, c.config.ClientSecret,
		resolveTokenAuthMethod(discovered.Server, c.config.ClientSecret))
}

// Refresh exchanges the refresh token on a previously issued TokenSet for a
// fresh one, reusing the client credentials that set carries. The secret is
// optional: a public client (one identified by a client ID metadata document, or
// registered without a secret) refreshes with a token_endpoint_auth_method of
// none. The configured credentials stand in when the set names no client, and
// credentials travel as a pair, so the configured secret is only used for the
// configured client id and is never attached to a different one.
//
// The set is refused unless the issuer it records is the authorization server
// the protected resource currently points at. A refresh token and the client
// secret that obtained it are only ever meaningful to the server that issued
// them, so a resource that starts advertising a different authorization server
// must not be able to have them sent there. The returned error wraps
// ErrIssuerMismatch; see that error for what recovers from it.
//
// A set with nothing to present is reported as such before any network call,
// with an error wrapping ErrNoRefreshToken.
func (c *Client) Refresh(ctx context.Context, token *TokenSet) (*TokenSet, error) {
	if token == nil || token.RefreshToken == "" {
		return nil, wrapError(ErrNoRefreshToken, "refreshing needs the token set a grant issued")
	}
	discovered, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireSameIssuer(token.Issuer, discovered.Server.Issuer); err != nil {
		return nil, err
	}

	clientID, clientSecret := token.ClientID, token.ClientSecret
	if clientID == "" {
		clientID = c.config.ClientID
	}
	if clientSecret == "" && clientID == c.config.ClientID {
		clientSecret = c.config.ClientSecret
	}
	// No scope is sent. A refresh may not ask for more than was granted, an
	// omitted scope means exactly what was granted (RFC 6749 6), and the scope
	// a new authorization would select is not a record of what this one got.
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {token.RefreshToken},
		"resource":      {c.resourceURL},
	}
	tok, err := c.requestToken(ctx, discovered.Server.Issuer, discovered.Server.TokenEndpoint, form,
		clientID, clientSecret, resolveTokenAuthMethod(discovered.Server, clientSecret))
	if err != nil {
		return nil, err
	}
	tok.ClientID = clientID
	tok.ClientSecret = clientSecret
	return tok, nil
}

// requireSameIssuer asserts that credentials issued by bound may be presented to
// current. A set recording no issuer cannot be shown to belong to the current
// server, so it is refused too rather than sent on trust.
func requireSameIssuer(bound, current string) error {
	if bound == "" {
		return wrapError(ErrIssuerMismatch, "the token set does not record the authorization server that issued it")
	}
	if subtle.ConstantTimeCompare([]byte(bound), []byte(current)) != 1 {
		return wrapError(ErrIssuerMismatch, "the token set was issued by [%s], but the resource now points at [%s]", bound, current)
	}
	return nil
}

// requireConfiguredIssuer asserts that a flow targeting issuer is a flow this
// client is configured for. A Config that names an issuer names the only
// authorization server this client talks to, whichever one the protected
// resource currently advertises; a Config that names none leaves the choice to
// discovery.
func (c *Client) requireConfiguredIssuer(issuer string) error {
	if c.config.Issuer == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(c.config.Issuer), []byte(issuer)) != 1 {
		return wrapError(ErrIssuerMismatch, "the configured client credentials belong to [%s], but this flow targets [%s]", c.config.Issuer, issuer)
	}
	return nil
}

// requireConfiguredCredentialsBelongTo asserts that the client credentials held
// in Config may be presented to issuer, which they may only be when Config.Issuer
// names that server. It applies to the configured client id as much as to the
// configured secret: a pre-registered identifier exists at the one authorization
// server it was registered with, and the MCP authorization specification has
// such credentials keyed by that issuer. Presented anywhere else, even a public
// identifier walks the user to a consent screen for this application's identity
// at a server the application has never dealt with.
//
// Credentials an application holds out of band carry no issuer of their own, and
// the protected resource that would supply one is the very party the binding
// has to hold against, so an unconfigured issuer is refused rather than taken
// from whichever server was seen first: a binding learned that way is only as
// durable as the client holding it, and a process that builds a client per
// request holds it for one request. Credentials issued by dynamic registration
// need no rule of their own, since a client record is memoized against the
// authorization server that issued it and dropped as soon as discovery reports
// another, and a client ID metadata document is fetched by whichever server it
// is shown to.
func (c *Client) requireConfiguredCredentialsBelongTo(issuer, clientID, clientSecret string) error {
	if !c.presentsConfigured(c.config.ClientID, clientID) && !c.presentsConfigured(c.config.ClientSecret, clientSecret) {
		return nil
	}
	if c.config.Issuer == "" {
		return wrapError(ErrIssuerRequired, "the configured client credentials cannot be presented to [%s]", issuer)
	}
	return c.requireConfiguredIssuer(issuer)
}

// presentsConfigured reports whether presented is the configured credential. An
// empty configured value is no credential, so it matches nothing, an empty
// presented value included.
func (c *Client) presentsConfigured(configured, presented string) bool {
	return configured != "" && subtle.ConstantTimeCompare([]byte(configured), []byte(presented)) == 1
}

// requestToken posts a token request to the token endpoint of issuer, applying
// the chosen client-authentication method, and parses the resulting TokenSet.
// The set records issuer, which binds it to the server that issued it for a
// later refresh.
func (c *Client) requestToken(ctx context.Context, issuer, tokenEndpoint string, form url.Values, clientID, clientSecret, authMethod string) (*TokenSet, error) {
	if err := c.requireConfiguredCredentialsBelongTo(issuer, clientID, clientSecret); err != nil {
		return nil, err
	}

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
	if err := requireBearerTokenType(data); err != nil {
		return nil, err
	}
	tok := tokenSetFromResponse(data, c.now())
	tok.Issuer = issuer
	return &tok, nil
}

// resolveRegistration determines the client identity for a flow that has no
// configured client_id, in the order the MCP authorization specification
// prescribes: the client ID metadata document when the authorization server
// supports it (the server fetches the document, so the application stays a
// public client with no secret), then dynamic client registration. A dynamic
// registration is memoized while it stays usable, so starting another
// authorization reuses the issued client instead of creating a fresh record on
// the server every time. The document is preferred on every flow, so a server
// that starts advertising support stops the client from reusing a record it
// registered earlier.
//
// The gate is held across the registration request, so a burst of concurrent
// first flows creates exactly one client record rather than one per flow.
func (c *Client) resolveRegistration(ctx context.Context, discovered *DiscoveryResult, redirectURI string) (*ClientRegistration, error) {
	metadata := discovered.Server
	if documentURL := c.clientIDMetadataURL(metadata); documentURL != "" {
		return &ClientRegistration{ClientID: documentURL}, nil
	}

	issuedBy := registrationIdentity(metadata)
	if reg := c.memoizedRegistration(issuedBy); reg != nil {
		return reg, nil
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	// Another flow may have registered while this one waited for the gate.
	if reg := c.memoizedRegistration(issuedBy); reg != nil {
		return reg, nil
	}

	if metadata.RegistrationEndpoint == "" {
		return nil, newError("no client_id was configured, no usable client ID metadata document URL is available, and the authorization server does not support dynamic client registration")
	}
	reg, err := registerClient(ctx, c.httpClient, metadata.RegistrationEndpoint, redirectURI,
		c.resolveScope(discovered), applicationType(redirectURI), resolveTokenAuthMethod(metadata, "confidential"))
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.registration, c.registeredAt, c.registeredWith = reg, c.now(), issuedBy
	c.mu.Unlock()
	return reg, nil
}

// memoizedRegistration returns the client record registered earlier while it is
// still usable: issued by the authorization server identified by issuedBy, with
// a secret that has not expired, and registered less than registrationTTL ago.
// It returns nil otherwise, which sends the caller on to register again.
func (c *Client) memoizedRegistration(issuedBy string) *ClientRegistration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registration == nil || c.registeredWith != issuedBy {
		return nil
	}
	now := c.now()
	if c.registration.Expired(now) || !now.Before(c.registeredAt.Add(registrationTTL)) {
		return nil
	}
	return c.registration
}

// registrationIdentity names the authorization server a client record was
// issued by. A record is only ever accepted by that server, so metadata naming a
// different issuer or registration endpoint describes a server this Client holds
// no identity with.
func registrationIdentity(metadata *AuthServerMetadata) string {
	return metadata.Issuer + "\n" + metadata.RegistrationEndpoint
}

// discover resolves the authorization-server metadata for a flow and asserts it
// describes the server this client is configured for.
func (c *Client) discover(ctx context.Context) (*DiscoveryResult, error) {
	result, err := c.discoverMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.requireConfiguredIssuer(result.Server.Issuer); err != nil {
		return nil, err
	}
	return result, nil
}

// discoverMetadata fetches the authorization-server metadata, reusing the
// previous result until discoveryTTL has elapsed. A failed discovery is not
// memoized.
func (c *Client) discoverMetadata(ctx context.Context) (*DiscoveryResult, error) {
	if result := c.memoizedDiscovery(); result != nil {
		return result, nil
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	// Another flow may have discovered while this one waited for the gate.
	if result := c.memoizedDiscovery(); result != nil {
		return result, nil
	}

	result, err := c.discovery.Discover(ctx, c.resourceURL, c.resourceMetadataURL)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.discovered, c.discoveredAt = result, c.now()
	c.mu.Unlock()
	return result, nil
}

// memoizedDiscovery returns the metadata discovered earlier while it is still
// within discoveryTTL, or nil when it has to be fetched again.
func (c *Client) memoizedDiscovery() *DiscoveryResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discovered != nil && c.now().Before(c.discoveredAt.Add(discoveryTTL)) {
		return c.discovered
	}
	return nil
}

// acquire takes the gate that serialises discovery and registration. A flow
// whose context ends while it waits gives up with that context's error instead
// of parking behind an authorization server answering another flow; the returned
// error unwraps to context.Canceled or context.DeadlineExceeded.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return wrapError(ctx.Err(), "gave up waiting for another flow to finish contacting the authorization server")
	}
}

// release hands the gate to the next waiting flow.
func (c *Client) release() { <-c.gate }

// resolveScope picks the scope a flow asks for, in the order of the MCP
// authorization specification's scope selection strategy: the scope the server
// named in its challenge, which is what the refused request needs, and failing
// that every scope the protected resource lists as supported. The scope the
// consumer configured stands between the two: it is the application's own
// statement of what it needs, so it outranks a list of everything on offer, and
// yields to a challenge that says what this request takes.
//
// It returns "" when none of them names a scope, and the parameter is then left
// out so the authorization server applies its default. A made-up scope is not a
// substitute: a server that does not know it answers invalid_scope.
func (c *Client) resolveScope(discovered *DiscoveryResult) string {
	if c.challengeScope != "" {
		return c.challengeScope
	}
	if c.config.Scope != "" {
		return c.config.Scope
	}
	return strings.Join(scopeTokens(discovered.ScopesSupported), " ")
}

// scopeTokens returns the entries of a scopes_supported list that can be
// requested, in the order given and without repeats. The list is written by the
// protected resource, so an entry is kept only when it is a scope-token as RFC
// 6749 3.3 defines one (printable ASCII other than the space, the double quote
// and the backslash): anything else would not survive as one scope in the
// space-delimited parameter it is joined into.
func scopeTokens(supported []string) []string {
	tokens := make([]string, 0, len(supported))
	for _, scope := range supported {
		if isScopeToken(scope) && !contains(tokens, scope) {
			tokens = append(tokens, scope)
		}
	}
	return tokens
}

// isScopeToken reports whether v is a scope-token: one or more characters from
// %x21 / %x23-5B / %x5D-7E (RFC 6749 3.3).
func isScopeToken(v string) bool {
	if v == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		if ch := v[i]; ch <= 0x20 || ch >= 0x7f || ch == '"' || ch == '\\' {
			return false
		}
	}
	return true
}

// validateState asserts that an authorization response answers the request the
// pending authorization was created for. A pending authorization that records
// no state is refused outright: AuthorizationURL always generates one, so an
// empty value means the record was built by hand or lost on the way through the
// caller's store, and two empty strings would otherwise compare equal and let a
// response that carries no state at all through.
func validateState(pending *PendingAuthorization, state string) error {
	if pending.State == "" {
		return newError("the pending OAuth authorization records no state, so no response can be matched to it")
	}
	if subtle.ConstantTimeCompare([]byte(pending.State), []byte(state)) != 1 {
		return newError("the OAuth state parameter did not match; possible CSRF attempt")
	}
	return nil
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
