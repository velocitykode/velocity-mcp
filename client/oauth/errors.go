package oauth

import "fmt"

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
