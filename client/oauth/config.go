package oauth

// Config holds the static OAuth client configuration supplied by the consumer.
// When ClientID is empty the client attempts dynamic client registration against
// the authorization server, and when Scope is empty the challenge scope (or the
// scopes the resource lists as supported) is used. Only Issuer is conditionally
// required: a configured ClientID, with or without a ClientSecret, cannot be
// presented without it.
type Config struct {
	// ClientID is the pre-registered OAuth client identifier, if any.
	// Presenting it requires Issuer.
	ClientID string
	// ClientSecret is the client secret for confidential clients, if any.
	// Presenting it requires Issuer.
	ClientSecret string
	// Issuer is the authorization server these configured credentials were
	// issued by, spelled exactly as that server's metadata advertises it (RFC
	// 8414 section 3.3 compares issuers by simple string comparison).
	//
	// It is required whenever ClientID or ClientSecret is set, and it is the
	// only thing that binds credentials an application holds out of band to the
	// server that issued them: the protected resource decides which
	// authorization server it advertises, so a resource that is compromised or
	// hijacked can point a client at a server of its choosing, and a binding
	// learned from the resource would move with it. The configured credentials
	// are presented to Issuer and to no other server, whichever one discovery
	// currently reports. That holds for a public client too: its identifier is
	// no secret, but it exists at the one server it was registered with.
	//
	// When it is set it pins the whole client. Leave it empty only when no
	// ClientID is configured, that is for a client ID metadata document and for
	// dynamic client registration, where the identity is issued by or shown to
	// whichever server discovery reports and is dropped as soon as that changes.
	Issuer string
	// Scope is the scope this application needs. It is requested unless the
	// server's WWW-Authenticate challenge names a scope of its own, which says
	// what the refused request takes and therefore wins. When both are empty the
	// flow asks for every scope the protected resource lists in its
	// scopes_supported metadata, and when it lists none the scope parameter is
	// left out so the authorization server applies its default.
	Scope string
	// RedirectURI is the callback URL the authorization server redirects to
	// after the user authorizes. Required for the authorization-code grant.
	RedirectURI string
	// ClientIDMetadataURL is the HTTPS URL of this application's client ID
	// metadata document. When ClientID is empty and the authorization server
	// advertises client_id_metadata_document_supported, the URL itself is used
	// as the client_id and the application acts as a public client (no secret),
	// so no client record is created on the server. It is ignored when it is not
	// usable as a client identifier: non-HTTPS, no path component, a query or
	// fragment, or a host that is not publicly resolvable.
	ClientIDMetadataURL string
	// AllowPrivateHosts permits plain-HTTP and private/internal hosts for the
	// resource and authorization-server endpoints. The default (false) keeps
	// the RFC-aligned posture: HTTPS everywhere, localhost excepted, no
	// private or reserved addresses - guarding against server-advertised SSRF
	// targets. That last part is enforced as each connection is opened: the
	// host is resolved, refused when any of its addresses is loopback, private,
	// link-local or otherwise internal, and connected to at the address that
	// was checked, so a public-looking name that resolves inward gets nowhere
	// and neither does DNS rebinding. The one exemption is a resource that is
	// itself configured on localhost, 127.0.0.1 or ::1, which may name those
	// same hosts. Enable ONLY when the consumer itself vouches for the endpoint
	// (a self-hosted deployment on a private network, containerized
	// development where the server is not reachable via localhost). Never
	// enable it for endpoints supplied by untrusted parties: it lifts the
	// connection guard along with the URL checks.
	AllowPrivateHosts bool
}
