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
	// AllowPrivateHosts permits plain-HTTP and private/internal hosts for the
	// resource and authorization-server endpoints. The default (false) keeps
	// the RFC-aligned posture: HTTPS everywhere, localhost excepted, no
	// private or reserved addresses - guarding against server-advertised SSRF
	// targets. Enable ONLY when the consumer itself vouches for the endpoint
	// (a self-hosted deployment on a private network, containerized
	// development where the server is not reachable via localhost). Never
	// enable it for endpoints supplied by untrusted parties.
	AllowPrivateHosts bool
}
