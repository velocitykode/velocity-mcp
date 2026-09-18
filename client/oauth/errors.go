package oauth

import (
	"errors"
	"fmt"
)

// ErrPKCERequired reports that an authorization-code flow cannot start because
// the authorization server does not guarantee PKCE with the S256 code challenge
// method, which the MCP authorization specification requires of every client.
// It is wrapped by the *Error returned from AuthorizationURL, so callers match
// it with errors.Is. There is no opt-out.
var ErrPKCERequired = errors.New("PKCE with the S256 code challenge method is required")

// ErrIssuerMismatch reports that credentials were about to be presented to an
// authorization server other than the one they belong to, which happens when the
// protected resource starts advertising a different authorization server. It is
// wrapped by the *Error returned from any flow that would have carried
// credentials to such a server, so callers match it with errors.Is.
//
// What recovers from it depends on which credential was refused. A TokenSet
// issued by another server is replaced by authorizing again against the server
// the resource now points at. Credentials taken from Config carry no such
// remedy: they are presented to Config.Issuer and to no other server, so an
// application whose authorization server has genuinely moved registers with the
// new one and is reconfigured for it. Authorizing again with the old
// configuration is refused for as long as the configuration names the old
// server, which is what keeps a resource from choosing where those credentials
// travel.
var ErrIssuerMismatch = errors.New("the credentials belong to a different authorization server")

// ErrIssuerRequired reports that a flow would have presented the client
// credentials held in Config (the pre-registered client id, with or without a
// secret) without Config.Issuer naming the server that issued them, which
// leaves nothing to say they belong where the flow is pointed. It is a
// configuration fault rather than a live mismatch, so it does not wrap
// ErrIssuerMismatch, and callers separate the two with errors.Is: this one is
// answered by setting Config.Issuer, that one by re-registering with the server
// the resource now advertises.
var ErrIssuerRequired = errors.New("Config.Issuer must name the authorization server the configured credentials belong to")

// ErrNoRefreshToken reports that Refresh was handed no token set, or one that
// carries no refresh token and therefore nothing to present. It is wrapped by
// the *Error returned from Refresh, so callers match it with errors.Is and fall
// back to a fresh authorization rather than treating it as a server failure.
var ErrNoRefreshToken = errors.New("the token set carries no refresh token")

// Error is an OAuth-related failure raised by discovery, registration, or a
// token request. It optionally wraps an underlying transport error.
type Error struct {
	Message string
	Err     error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil oauth error>"
	}
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// Unwrap exposes the wrapped error for errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// newError builds an *Error from a printf-style message.
func newError(format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...)}
}

// wrapError builds an *Error carrying both a message and an underlying cause.
func wrapError(err error, format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...), Err: err}
}

// AuthorizationRequiredError signals that the MCP server requires OAuth
// authorization (it responded with HTTP 401/403). It carries the parsed
// WWW-Authenticate challenge so the caller can begin discovery from the
// advertised protected-resource metadata URL and scope.
type AuthorizationRequiredError struct {
	Message   string
	Challenge *Challenge
}

// Error implements the error interface.
func (e *AuthorizationRequiredError) Error() string {
	if e == nil {
		return "<nil authorization required error>"
	}
	return e.Message
}

// ResourceMetadataURL returns the protected-resource metadata URL advertised in
// the challenge, or the empty string when none was present.
func (e *AuthorizationRequiredError) ResourceMetadataURL() string {
	if e == nil || e.Challenge == nil {
		return ""
	}
	return e.Challenge.ResourceMetadataURL
}

// Scope returns the scope advertised in the challenge, or the empty string.
func (e *AuthorizationRequiredError) Scope() string {
	if e == nil || e.Challenge == nil {
		return ""
	}
	return e.Challenge.Scope
}
