package client

import (
	"bytes"
	"encoding/json"
	"slices"
)

// This file implements the client half of a multi-round-trip request. A server
// that needs more input before it can answer replies with a result whose
// resultType is "input_required", carrying the inputs it wants and an opaque
// state token. That reply is not the answer to the call, so it is surfaced as
// an UnfinishedResultError rather than decoded as an empty success; the caller
// gathers what was asked for and calls again with a Continuation. Any other
// result the specification does not define is invalid, and is reported as a
// failed call rather than as something to continue.

// resultTypeComplete marks a result that carries the whole answer. A result
// that states no resultType at all predates the member and is complete too.
// resultTypeInputRequired marks the one unfinished result the protocol defines:
// the server wants further input before it can answer. The specification states
// that a result type the client does not recognize is invalid, and this client
// negotiates no extension that would define another, so these two and the
// absent member are the whole recognized set.
const (
	resultTypeComplete      = "complete"
	resultTypeInputRequired = "input_required"
)

// answerableInputMethods are the requests a server may ask the client to answer
// in an unfinished result. The specification names these three and no others,
// so an entry under any other method is not an input this client can gather.
var answerableInputMethods = []string{
	"elicitation/create",
	"sampling/createMessage",
	"roots/list",
}

// UnfinishedResultError reports that the server answered a request without
// completing it. It carries what the server asked for, so the caller can gather
// those inputs and repeat the request with a Continuation.
//
// The retry is a new request with its own id: the exchange that produced this
// error is over, and nothing about it other than the values below carries into
// the next one.
type UnfinishedResultError struct {
	// Method is the request the server declined to complete.
	Method string
	// ResultType is the kind of unfinished result the server sent, verbatim.
	// "input_required" is the one this revision of the protocol defines, and a
	// result stating anything else is refused rather than reported here.
	ResultType string
	// InputRequests holds the requests the server wants answered, keyed by the
	// identifiers it assigned, each in the form it arrived in. Every one is a
	// request object under a method a client can answer. A caller answers them
	// under those same keys in Continuation.InputResponses. It is empty when the
	// server asked for nothing and the request may simply be repeated with the
	// state token.
	InputRequests map[string]json.RawMessage
	// RequestState is the server's opaque state token, echoed back unchanged in
	// the Continuation. It is meaningless to the client and is never inspected.
	// It is nil when the result stated no token at all, which is a different
	// thing from a token that is the empty string: the specification requires
	// the member to be sent back exactly as it arrived and omitted only when it
	// did not arrive.
	RequestState *string
	// Result is the whole unfinished result as it arrived, for a caller that
	// needs a member this type does not name.
	Result json.RawMessage
}

// Error implements the error interface.
func (e *UnfinishedResultError) Error() string {
	if e == nil {
		return "<nil unfinished result error>"
	}
	return "the server did not complete [" + e.Method + "]: it answered with a [" +
		e.ResultType + "] result and is waiting for further input"
}

// Continue builds the continuation that repeats the request, carrying the
// answers to what the server asked for and its state token exactly as it
// arrived. It is the faithful way to retry: a token the server sent as an empty
// string is echoed as one, and a result that carried no token repeats the
// request without the member.
func (e *UnfinishedResultError) Continue(responses map[string]any) Continuation {
	if e == nil {
		return Continuation{InputResponses: responses}
	}
	return Continuation{RequestState: e.RequestState, InputResponses: responses}
}

// Continuation carries what the client owes the server after an unfinished
// result: the responses to the inputs it asked for, and the state token it
// handed out. Pass it to the call that repeats the request.
type Continuation struct {
	// RequestState is the token from the unfinished result, echoed exactly.
	// A nil token is omitted from the retry, which is what a result that
	// carried none requires; a non-nil one is sent as it stands, empty string
	// included.
	RequestState *string
	// InputResponses answers UnfinishedResultError.InputRequests under the same
	// keys. It may be empty when the server asked for nothing.
	InputResponses map[string]any
}

// checkContinuation reports a request given more than one continuation: two
// would describe two different retries of the same call. It is checked before
// anything the request would otherwise do, so a request no client could mean
// costs the wire nothing.
func checkContinuation(continuation []Continuation) error {
	if len(continuation) > 1 {
		return newError("at most one continuation may be supplied per request")
	}
	return nil
}

// applyContinuation folds an optional continuation into a request's params.
func applyContinuation(params map[string]any, continuation []Continuation) error {
	if err := checkContinuation(continuation); err != nil {
		return err
	}
	if len(continuation) == 0 {
		return nil
	}
	if state := continuation[0].RequestState; state != nil {
		params["requestState"] = *state
	}
	if responses := continuation[0].InputResponses; len(responses) > 0 {
		params["inputResponses"] = responses
	}
	return nil
}

// unfinishedResult reports a result the server did not complete, or nil for one
// that carries the whole answer. A result is never read as an answer unless it
// says it is one: an unfinished result decoded as a success would report a call
// that never ran as a call that returned nothing.
//
// Only the two result types the specification defines are recognized. Anything
// else the server states, a result type that is not a string included, is
// invalid and is reported as such rather than as an unfinished result: an
// invalid result tells the caller nothing about how to continue, and offering
// it as a continuation would have the caller repeat the call as if the server
// had asked it to.
func unfinishedResult(method string, raw json.RawMessage) error {
	members, ok := jsonObject(raw)
	if !ok {
		return nil
	}
	stated, present := members["resultType"]
	if !present {
		return nil
	}
	kind, isString := jsonString(stated)
	if !isString {
		return invalidResult(method, "its result type is not a string")
	}
	switch kind {
	case resultTypeComplete:
		return nil
	case resultTypeInputRequired:
		return inputRequired(method, members, raw)
	default:
		return invalidResult(method, "it states the unrecognized result type ["+kind+"]")
	}
}

// inputRequired reads a result the server suspended into the error its caller
// continues from, or reports the result invalid.
//
// The specification constrains what such a result may carry, and each of the
// constraints is what makes the retry meaningful: a state token is an opaque
// string, echoed back exactly as it arrived; every input request is a request
// object under one of the methods a client can answer; and a result states at
// least one of the two, since a result stating neither asks for nothing and
// leaves the retry identical to the call that was already refused.
func inputRequired(method string, members map[string]json.RawMessage, raw json.RawMessage) error {
	err := &UnfinishedResultError{Method: method, ResultType: resultTypeInputRequired, Result: raw}

	if stated, present := members["requestState"]; present {
		state, isString := jsonString(stated)
		if !isString {
			return invalidResult(method, "its state token is not a string")
		}
		err.RequestState = &state
	}

	if stated, present := members["inputRequests"]; present {
		requests, isObject := jsonObject(stated)
		if !isObject {
			return invalidResult(method, "its input requests are not an object")
		}
		for key, request := range requests {
			if !answerableInputRequest(request) {
				return invalidResult(method, "its input request ["+key+"] is not a request this client can answer")
			}
		}
		err.InputRequests = requests
	}

	if err.RequestState == nil && len(err.InputRequests) == 0 {
		return invalidResult(method, "it carries neither input requests nor a state token")
	}
	return err
}

// answerableInputRequest reports whether one entry of an unfinished result's
// input requests is a request object the client could gather an answer for.
func answerableInputRequest(raw json.RawMessage) bool {
	members, isObject := jsonObject(raw)
	if !isObject {
		return false
	}
	method, isString := jsonString(members["method"])
	return isString && slices.Contains(answerableInputMethods, method)
}

// invalidResult reports a result the server had no business sending. It is a
// plain client error rather than an UnfinishedResultError, so a caller reading
// the outcome with errors.As is never handed a continuation built out of a
// result the protocol does not allow.
func invalidResult(method, reason string) error {
	return newError("the server answered [" + method + "] with an invalid result: " + reason)
}

// jsonObject decodes a JSON object into its members, each kept in the form it
// arrived in, reporting false for anything that is not an object. Members stay
// raw so a value no Go type can hold (a number outside float64's range, say)
// costs only itself rather than the whole decode.
func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil || members == nil {
		return nil, false
	}
	return members, true
}

// jsonString decodes a member the protocol models as a string, reporting false
// for a member that is absent or carries any other JSON type. The token is
// inspected first because encoding/json reads a JSON null into a string without
// complaint.
func jsonString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}
