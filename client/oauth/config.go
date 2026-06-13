package oauth

// Config holds the static OAuth client configuration supplied by the consumer.
// All fields are optional: when ClientID is empty the client attempts dynamic
// client registration against the authorization server, and when Scope is empty
// the challenge scope (or a default) is used.
type Config struct {
	// ClientID is the pre-registered OAuth client identifier, if any.
	ClientID string
	// ClientSecret is the client secret for confidential clients, if any.
	ClientSecret string
	// Scope is the requested scope. When empty the WWW-Authenticate challenge
	// scope is used, falling back to "mcp:use".
	Scope string
	// RedirectURI is the callback URL the authorization server redirects to
	// after the user authorizes. Required for the authorization-code grant.
	RedirectURI string
}
