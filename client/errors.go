package client

import (
	"errors"
	"strconv"
)

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

// TransportError reports a failure of the transport channel itself: the
// endpoint refused the exchange, the subprocess went away, and so on. It is
// also a client error, so errors.As finds an *Error in its chain.
//
// The connection probe treats a transport failure as "this endpoint would not
// take the request" rather than "the server refused it", and retries the
// exchange with the older handshake. A custom transport signals that same
// meaning by returning one of these.
type TransportError struct {
	cause *Error
}

// NewTransportError builds a transport failure with a message and an optional
// underlying cause.
func NewTransportError(message string, cause error) *TransportError {
	return &TransportError{cause: &Error{Message: message, Err: cause}}
}

// Error implements the error interface.
func (e *TransportError) Error() string {
	if e == nil {
		return "<nil client transport error>"
	}
	return e.cause.Error()
}

// Unwrap exposes the underlying client error (and through it any cause) for
// errors.Is/As.
func (e *TransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// TimeoutError reports that the transport gave up waiting for a server
// response. A timeout is a transport failure: errors.As finds a
// *TransportError (and an *Error) in its chain.
type TimeoutError struct {
	cause *TransportError
}

// NewTimeoutError builds a timeout failure with a message and an optional
// underlying cause (a context error, for instance).
func NewTimeoutError(message string, cause error) *TimeoutError {
	return &TimeoutError{cause: NewTransportError(message, cause)}
}

// Error implements the error interface.
func (e *TimeoutError) Error() string {
	if e == nil {
		return "<nil client timeout error>"
	}
	return e.cause.Error()
}

// Unwrap exposes the underlying transport error for errors.Is/As.
func (e *TimeoutError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// unanswered marks, in the chain of a transport failure, that the peer gave no
// answer at all: the request timed out, or the connection failed under it. It
// is what tells a failure that says something about the server from one that
// says nothing. An HTTP endpoint that turns a request away has answered, and a
// client may put the request another way; one that did not answer in time has
// said nothing of what it speaks, and a client that read its silence as the
// answer of an older server would settle the older handshake with a server
// that speaks the newer one. The message and the chain are those of the
// failure it marks.
type unanswered struct{ err error }

// Error implements the error interface.
func (u *unanswered) Error() string {
	if u == nil || u.err == nil {
		return "the peer gave no answer"
	}
	return u.err.Error()
}

// Unwrap exposes the failure itself for errors.Is/As.
func (u *unanswered) Unwrap() error {
	if u == nil {
		return nil
	}
	return u.err
}

// isUnanswered reports whether a failure carries the mark.
func isUnanswered(err error) bool {
	var mark *unanswered
	return errors.As(err, &mark)
}

// quotedLimit is how much of a text the server sent an error quotes.
const quotedLimit = 256

// quoted renders text the server sent for an error message, cut to quotedLimit
// bytes. A server names things, in the frames it sends and on its standard
// error, and an error that quoted them verbatim would hand the server whatever
// renders the error: a terminal acts on a control character and a log reader
// on a line break. The text is escaped the way a Go string literal is, with
// every control character and every byte that is not valid text written out as
// an escape, so what the server sent is reported and never acted on.
func quoted(text string) string {
	if len(text) <= quotedLimit {
		return escaped(text)
	}
	return escaped(text[:quotedLimit]) + "..."
}

// escaped renders text with its control characters, and every other character
// that is not printable, written as escapes. It is the body of a Go string
// literal for the text: the quotes a literal has around it are left off.
func escaped(text string) string {
	literal := strconv.Quote(text)
	return literal[1 : len(literal)-1]
}
