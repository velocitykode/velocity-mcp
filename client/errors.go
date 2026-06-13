package client

import "errors"

// Error is a client-side protocol or transport failure (as opposed to a
// jsonrpc.Error returned by the server, which surfaces unchanged). It optionally
// wraps an underlying cause.
type Error struct {
	Message string
	Err     error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil client error>"
	}
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// newError builds an *Error with a static message.
func newError(message string) *Error { return &Error{Message: message} }

// wrapError builds an *Error carrying a message and an underlying cause.
func wrapError(err error, message string) *Error { return &Error{Message: message, Err: err} }

// errSessionExpired is a sentinel signalling that the server reported the
// session as expired (HTTP 404 after a session was established). The protocol
// catches it to transparently reconnect and retry once.
var errSessionExpired = errors.New("client: session expired")
