package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file covers the client half of a multi-round-trip request: a result the
// server did not complete must never be handed back as an answer, and the
// continuation the caller repeats the request with must reach the wire exactly
// as the server asked for it.

// inputRequiredFrame builds an unfinished result carrying the given members
// beside its resultType.
func inputRequiredFrame(members map[string]any) string {
	result := map[string]any{"resultType": "input_required"}
	for key, value := range members {
		result[key] = value
	}
	return resultFrame(result)
}

// stateToken returns a continuation token that is present, which is what tells
// a token the server sent apart from one it never sent.
func stateToken(value string) *string { return &value }

// renderState renders a state token for a failure message, naming an absent one
// rather than printing the address of a present one.
func renderState(state *string) string {
	if state == nil {
		return "absent"
	}
	return strconv.Quote(*state)
}

// elicitationRequest is an inputRequests entry asking the client for a value.
func elicitationRequest() map[string]any {
	return map[string]any{
		"method": "elicitation/create",
		"params": map[string]any{
			"mode":    "form",
			"message": "Please provide your username",
		},
	}
}

// TestUnfinishedResultIsNotReadAsAnAnswer asserts each of the three requests a
// server may leave unfinished surfaces the outcome rather than an empty
// success, and that the continuation state reaches the caller intact.
func TestUnfinishedResultIsNotReadAsAnAnswer(t *testing.T) {
	const state = "AEAD-protected blob"

	tests := []struct {
		name   string
		method string
		// prelude answers whatever the call reads before the request itself.
		prelude []string
		call    func(*Client) error
	}{
		{
			name:    "tools/call",
			method:  "tools/call",
			prelude: []string{emptyToolsFrame()},
			call: func(c *Client) error {
				_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"query": "select 1"})
				return err
			},
		},
		{
			name:   "resources/read",
			method: "resources/read",
			call: func(c *Client) error {
				_, err := c.ReadResource(context.Background(), "file:///notes.txt")
				return err
			},
		},
		{
			name:   "prompts/get",
			method: "prompts/get",
			call: func(c *Client) error {
				_, err := c.GetPrompt(context.Background(), "review", nil)
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := discoveryClient(t, append(tc.prelude, inputRequiredFrame(map[string]any{
				"requestState":  state,
				"inputRequests": map[string]any{"github_login": elicitationRequest()},
			}))...)

			err := tc.call(c)
			if err == nil {
				t.Fatal("an unfinished result was reported as a completed call")
			}
			var unfinished *UnfinishedResultError
			if !errors.As(err, &unfinished) {
				t.Fatalf("error = %v (%T), want an unfinished result", err, err)
			}
			if unfinished.Method != tc.method {
				t.Fatalf("method = %q, want %q", unfinished.Method, tc.method)
			}
			if unfinished.ResultType != "input_required" {
				t.Fatalf("resultType = %q, want input_required", unfinished.ResultType)
			}
			if unfinished.RequestState == nil || *unfinished.RequestState != state {
				t.Fatalf("requestState = %s, want %q", renderState(unfinished.RequestState), state)
			}
			request, present := unfinished.InputRequests["github_login"]
			if !present {
				t.Fatalf("inputRequests = %v, want the github_login entry", unfinished.InputRequests)
			}
			if !strings.Contains(string(request), `"elicitation/create"`) {
				t.Fatalf("inputRequests[github_login] = %s", request)
			}
			if !strings.Contains(err.Error(), "did not complete ["+tc.method+"]") {
				t.Fatalf("message = %q", err.Error())
			}
		})
	}
}

// TestUnfinishedResultWithStateAlone asserts the shape the specification
// permits when the server wants no input at all: a state token on its own,
// which a client may repeat the request with immediately. It is the shape that
// reaches a client declaring no sampling or elicitation capability.
func TestUnfinishedResultWithStateAlone(t *testing.T) {
	c, _ := discoveryClient(t, emptyToolsFrame(), inputRequiredFrame(map[string]any{"requestState": "opaque"}))

	_, err := c.CallTool(context.Background(), "execute_sql", nil)
	var unfinished *UnfinishedResultError
	if !errors.As(err, &unfinished) {
		t.Fatalf("error = %v, want an unfinished result", err)
	}
	if unfinished.RequestState == nil || *unfinished.RequestState != "opaque" {
		t.Fatalf("requestState = %s, want opaque", renderState(unfinished.RequestState))
	}
	if len(unfinished.InputRequests) != 0 {
		t.Fatalf("inputRequests = %v, want none", unfinished.InputRequests)
	}
}

// TestContinuationRepeatsTheRequest asserts the retry carries what the server
// asked for: the state token verbatim, the responses under the server's own
// keys, the original params, and a fresh request id.
func TestContinuationRepeatsTheRequest(t *testing.T) {
	c, s := discoveryClient(t,
		emptyToolsFrame(),
		inputRequiredFrame(map[string]any{"requestState": "opaque"}),
		toolCallFrame("done"),
	)

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"query": "select 1"})
	var unfinished *UnfinishedResultError
	if !errors.As(err, &unfinished) {
		t.Fatalf("error = %v, want an unfinished result", err)
	}

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"query": "select 1"}, Continuation{
		RequestState:   unfinished.RequestState,
		InputResponses: map[string]any{"github_login": map[string]any{"action": "accept"}},
	})
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}

	first, retry := s.frame(t, 2), s.frame(t, 3)
	if retry.Params["requestState"] != "opaque" {
		t.Fatalf("requestState = %v, want opaque", retry.Params["requestState"])
	}
	responses, ok := retry.Params["inputResponses"].(map[string]any)
	if !ok || responses["github_login"] == nil {
		t.Fatalf("inputResponses = %v", retry.Params["inputResponses"])
	}
	if retry.Params["name"] != "execute_sql" {
		t.Fatalf("the retry lost the original params: %v", retry.Params)
	}
	if string(first.ID) == string(retry.ID) {
		t.Fatalf("the retry reused the request id %s", retry.ID)
	}
}

// TestContinuationSendsNothingItWasNotGiven asserts the two members are omitted
// when the caller has neither, which is what a result carrying no state
// requires of the retry.
func TestContinuationSendsNothingItWasNotGiven(t *testing.T) {
	c, s := discoveryClient(t, emptyToolsFrame(), toolCallFrame("done"))

	if _, err := c.CallTool(context.Background(), "execute_sql", nil, Continuation{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	params := s.frame(t, 2).Params
	for _, member := range []string{"requestState", "inputResponses"} {
		if _, present := params[member]; present {
			t.Fatalf("the retry carried [%s] though it was given none: %v", member, params)
		}
	}
}

// TestContinuationEchoesTheStateTokenExactly asserts the round trip of the
// state token, which the specification requires back exactly as it arrived and
// omitted only when it did not arrive. An empty token is a token: a retry that
// dropped it would be a retry the server cannot match to the exchange it left
// unfinished, and the caller would have no way to send it through the public
// API. A result that asks for inputs and states no token is retried without
// the member, which the specification requires of the client.
func TestContinuationEchoesTheStateTokenExactly(t *testing.T) {
	tests := []struct {
		name string
		// members are the members of the unfinished result beside its
		// resultType, as they arrive on the wire.
		members string
		// want is the token the client must report and send back, rendered as
		// a quoted string, or "absent" for no token at all.
		want string
	}{
		{name: "a token", members: `,"requestState":"opaque"`, want: `"opaque"`},
		{name: "an empty token", members: `,"requestState":""`, want: `""`},
		{name: "a unicode token", members: `,"requestState":"état-☕"`, want: `"état-☕"`},
		{
			name:    "no token at all",
			members: `,"inputRequests":{"github_login":{"method":"elicitation/create"}}`,
			want:    "absent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":` + scriptRequestID +
				`,"result":{"resultType":"input_required"` + tc.members + `}}`
			c, s := discoveryClient(t, emptyToolsFrame(), frame, toolCallFrame("done"))

			arguments := map[string]any{"query": "select 1"}
			_, err := c.CallTool(context.Background(), "execute_sql", arguments)
			var unfinished *UnfinishedResultError
			if !errors.As(err, &unfinished) {
				t.Fatalf("error = %v, want an unfinished result", err)
			}
			if got := renderState(unfinished.RequestState); got != tc.want {
				t.Fatalf("requestState = %s, want %s", got, tc.want)
			}

			result, err := c.CallTool(context.Background(), "execute_sql", arguments, unfinished.Continue(nil))
			if err != nil {
				t.Fatalf("continuation: %v", err)
			}
			if result.Text() != "done" {
				t.Fatalf("text = %q, want done", result.Text())
			}

			sent, present := s.frame(t, 3).Params["requestState"]
			got := "absent"
			if present {
				text, isString := sent.(string)
				if !isString {
					t.Fatalf("the retry carried requestState as %T, want a string", sent)
				}
				got = strconv.Quote(text)
			}
			if got != tc.want {
				t.Fatalf("the retry carried requestState %s, want %s", got, tc.want)
			}
		})
	}
}

// TestContinuationFromANilResultCarriesTheResponsesAlone asserts Continue is
// safe on a nil error, as the rest of this type's methods are: it answers with
// the responses it was handed and no token, rather than panicking in the middle
// of a caller's error handling.
func TestContinuationFromANilResultCarriesTheResponsesAlone(t *testing.T) {
	var unfinished *UnfinishedResultError
	continuation := unfinished.Continue(map[string]any{"github_login": map[string]any{"action": "accept"}})

	if continuation.RequestState != nil {
		t.Fatalf("requestState = %s, want absent", renderState(continuation.RequestState))
	}
	if continuation.InputResponses["github_login"] == nil {
		t.Fatalf("inputResponses = %v, want the responses it was given", continuation.InputResponses)
	}
}

// TestAtMostOneContinuation asserts two continuations are refused rather than
// silently reduced to one: they describe two different retries of one call.
func TestAtMostOneContinuation(t *testing.T) {
	c, s := discoveryClient(t, toolCallFrame("done"))

	_, err := c.CallTool(context.Background(), "x", nil, Continuation{RequestState: stateToken("a")}, Continuation{RequestState: stateToken("b")})
	if err == nil || !strings.Contains(err.Error(), "at most one continuation") {
		t.Fatalf("error = %v", err)
	}
	if got := s.methods(); slices.Contains(got, "tools/call") {
		t.Fatalf("the refused call reached the wire: %v", got)
	}
}

// FuzzUnfinishedResultState drives the reader of a server result over arbitrary
// bytes and holds it to what the specification states about a result offered as
// a continuation: it is an "input_required" result, it carries at least one of
// the two members that make a retry meaningful, and its state token reaches the
// retry exactly as the result stated it and only then. The result is whatever a
// server sent, so the reader must also never panic on it.
func FuzzUnfinishedResultState(f *testing.F) {
	seeds := []string{
		`{"resultType":"input_required","requestState":"opaque"}`,
		`{"resultType":"input_required","requestState":""}`,
		`{"resultType":"input_required","requestState":"état-☕"}`,
		`{"resultType":"input_required","requestState":"a\u0000b"}`,
		`{"resultType":"input_required"}`,
		`{"resultType":"input_required","requestState":null}`,
		`{"resultType":"input_required","requestState":7}`,
		`{"resultType":"input_required","requestState":{}}`,
		`{"resultType":"input_required","requestState":["a"]}`,
		`{"resultType":"input_required","requestState":"a","requestState":"b"}`,
		`{"resultType":"input_required","inputRequests":{"a":{"method":"elicitation/create"}}}`,
		`{"resultType":"input_required","inputRequests":{"a":{"method":"tools/call"}}}`,
		`{"resultType":"input_required","inputRequests":{"a":null}}`,
		`{"resultType":"input_required","inputRequests":{}}`,
		`{"resultType":"input_required","inputRequests":[]}`,
		`{"resultType":"partial","requestState":"opaque","inputRequests":{"a":{"method":"elicitation/create"}}}`,
		`{"resultType":7,"requestState":"opaque"}`,
		`{"resultType":"complete","requestState":"opaque"}`,
		`{"content":[]}`,
		`  {"resultType":"input_required","requestState":"opaque"}  `,
		`[]`,
		`{`,
		``,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		err := unfinishedResult("tools/call", raw)
		var unfinished *UnfinishedResultError
		if !errors.As(err, &unfinished) {
			return
		}
		if unfinished.ResultType != resultTypeInputRequired {
			t.Fatalf("%q was offered as a continuation of type %q", raw, unfinished.ResultType)
		}
		if unfinished.RequestState == nil && len(unfinished.InputRequests) == 0 {
			t.Fatalf("%q was offered as a continuation that asks for nothing and carries no state", raw)
		}

		params := map[string]any{}
		if applyErr := applyContinuation(params, []Continuation{unfinished.Continue(nil)}); applyErr != nil {
			t.Fatalf("the continuation of the result was refused: %v", applyErr)
		}
		sent, present := params["requestState"]

		// What the result stated, read without going through the code above.
		var members map[string]json.RawMessage
		if decodeErr := json.Unmarshal(raw, &members); decodeErr != nil {
			t.Fatalf("an unfinished result was read out of something that is not a JSON object: %q", raw)
		}
		member, stated := members["requestState"]
		want, wantPresent := "", false
		if trimmed := bytes.TrimSpace(member); stated && len(trimmed) > 0 && trimmed[0] == '"' {
			wantPresent = json.Unmarshal(trimmed, &want) == nil
		}

		if present != wantPresent {
			t.Fatalf("the retry of %q carries requestState = %v, want %v", raw, present, wantPresent)
		}
		if present && sent != want {
			t.Fatalf("the retry of %q carries requestState %q, want %q", raw, sent, want)
		}
	})
}

// TestCompleteResultsAreAnswers asserts the rule does not stand in the way of a
// finished call: a result that states "complete", and one from a server of an
// earlier revision that states no resultType at all, are both answers.
func TestCompleteResultsAreAnswers(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{name: "an explicit complete", frame: toolCallFrame("done")},
		{
			name: "no resultType at all",
			frame: resultFrame(map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "done"}},
			}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := discoveryClient(t, emptyToolsFrame(), tc.frame)
			result, err := c.CallTool(context.Background(), "execute_sql", nil)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.Text() != "done" {
				t.Fatalf("text = %q, want done", result.Text())
			}
		})
	}
}

// TestUnfinishedResultOnAnyMethod asserts an unfinished result on a request
// the server had no business suspending is still not read as an answer. The
// specification allows an unfinished result on three requests only, so one on a
// listing is a server misbehaving; reading it as an empty catalogue would report
// a server's tools as gone.
func TestUnfinishedResultOnAnyMethod(t *testing.T) {
	frame := `{"jsonrpc":"2.0","id":` + scriptRequestID +
		`,"result":{"resultType":"input_required","requestState":"opaque","tools":[]}}`
	c, _ := discoveryClient(t, frame)

	_, err := c.Tools(context.Background())
	var unfinished *UnfinishedResultError
	if !errors.As(err, &unfinished) {
		t.Fatalf("error = %v, want an unfinished result", err)
	}
	if unfinished.Method != "tools/list" {
		t.Fatalf("method = %q, want tools/list", unfinished.Method)
	}
	if unfinished.ResultType != "input_required" {
		t.Fatalf("resultType = %q, want input_required", unfinished.ResultType)
	}
}

// TestInvalidResultIsNotOfferedAsAContinuation asserts a result the
// specification does not define is reported as a failed call and never as
// something to continue from.
//
// The specification defines two result types and requires a client to treat any
// other value as invalid, and it constrains what an unfinished result may
// carry: an opaque string for the state token, and input requests that are
// request objects under the three methods a client can answer, at least one of
// the two being present. A client that read those results as continuations
// anyway would hand the caller an error that documents a retry, and the retry
// would repeat the original call carrying no state the server ever handed out.
func TestInvalidResultIsNotOfferedAsAContinuation(t *testing.T) {
	tests := []struct {
		name string
		// result is the result object as it arrives on the wire.
		result string
		// reason is the part of the message naming what was wrong with it.
		reason string
	}{
		{
			name:   "an unrecognized result type",
			result: `{"resultType":"partial","requestState":"opaque"}`,
			reason: "it states the unrecognized result type [partial]",
		},
		{
			name:   "a result type that is not a string",
			result: `{"resultType":7,"requestState":"opaque"}`,
			reason: "its result type is not a string",
		},
		{
			name:   "a null result type",
			result: `{"resultType":null,"requestState":"opaque"}`,
			reason: "its result type is not a string",
		},
		{
			name:   "a state token that is not a string",
			result: `{"resultType":"input_required","requestState":7}`,
			reason: "its state token is not a string",
		},
		{
			name:   "a null state token",
			result: `{"resultType":"input_required","requestState":null}`,
			reason: "its state token is not a string",
		},
		{
			name:   "neither input requests nor a state token",
			result: `{"resultType":"input_required"}`,
			reason: "it carries neither input requests nor a state token",
		},
		{
			name:   "no input request and no state token",
			result: `{"resultType":"input_required","inputRequests":{}}`,
			reason: "it carries neither input requests nor a state token",
		},
		{
			name:   "input requests that are not an object",
			result: `{"resultType":"input_required","requestState":"opaque","inputRequests":[]}`,
			reason: "its input requests are not an object",
		},
		{
			name:   "an input request that is not an object",
			result: `{"resultType":"input_required","requestState":"opaque","inputRequests":{"github_login":"elicitation/create"}}`,
			reason: "its input request [github_login] is not a request this client can answer",
		},
		{
			name:   "an input request with no method",
			result: `{"resultType":"input_required","requestState":"opaque","inputRequests":{"github_login":{"params":{}}}}`,
			reason: "its input request [github_login] is not a request this client can answer",
		},
		{
			name:   "an input request under a method the client cannot answer",
			result: `{"resultType":"input_required","requestState":"opaque","inputRequests":{"nested":{"method":"tools/call"}}}`,
			reason: "its input request [nested] is not a request this client can answer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":` + tc.result + `}`
			c, _ := discoveryClient(t, emptyToolsFrame(), frame)

			_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"query": "select 1"})
			if err == nil {
				t.Fatal("an invalid result was reported as a completed call")
			}
			var unfinished *UnfinishedResultError
			if errors.As(err, &unfinished) {
				t.Fatalf("an invalid result was offered as a continuation: %+v", unfinished)
			}
			var clientErr *Error
			if !errors.As(err, &clientErr) {
				t.Fatalf("error = %v (%T), want a client error", err, err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("message = %q, want it to name %q", err.Error(), tc.reason)
			}
			if !strings.Contains(err.Error(), "[tools/call]") {
				t.Fatalf("message = %q, want it to name the request", err.Error())
			}
			if !c.Connected() {
				t.Fatal("the client disconnected over an invalid result")
			}
		})
	}
}

// TestEveryAnswerableInputRequestIsAccepted asserts the three requests a server
// may ask a client to answer all reach the caller. Refusing one of them would
// leave a server unable to ask for input the specification allows it to ask for.
func TestEveryAnswerableInputRequestIsAccepted(t *testing.T) {
	requests := map[string]any{
		"github_login":      map[string]any{"method": "elicitation/create", "params": map[string]any{"mode": "form"}},
		"capital_of_france": map[string]any{"method": "sampling/createMessage", "params": map[string]any{"maxTokens": 100}},
		"workspace":         map[string]any{"method": "roots/list"},
	}
	c, _ := discoveryClient(t, emptyToolsFrame(), inputRequiredFrame(map[string]any{"inputRequests": requests}))

	_, err := c.CallTool(context.Background(), "execute_sql", nil)
	var unfinished *UnfinishedResultError
	if !errors.As(err, &unfinished) {
		t.Fatalf("error = %v, want an unfinished result", err)
	}
	if len(unfinished.InputRequests) != len(requests) {
		t.Fatalf("inputRequests = %v, want all %d entries", unfinished.InputRequests, len(requests))
	}
	if unfinished.RequestState != nil {
		t.Fatalf("requestState = %s, want absent", renderState(unfinished.RequestState))
	}
}

// TestUnfinishedResultKeepsTheWholeResult asserts the raw result travels with
// the error, so a caller can read a member this client does not name.
func TestUnfinishedResultKeepsTheWholeResult(t *testing.T) {
	c, _ := discoveryClient(t, emptyToolsFrame(), inputRequiredFrame(map[string]any{
		"requestState": "opaque",
		"vendorHint":   "retry in a moment",
	}))

	_, err := c.CallTool(context.Background(), "x", nil)
	var unfinished *UnfinishedResultError
	if !errors.As(err, &unfinished) {
		t.Fatalf("error = %v, want an unfinished result", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(unfinished.Result, &decoded); err != nil {
		t.Fatalf("result is not an object: %v", err)
	}
	if decoded["vendorHint"] != "retry in a moment" {
		t.Fatalf("result = %v, want the member the client does not name", decoded)
	}
}

// TestUnfinishedResultLeavesTheConnectionUp asserts the outcome is the server's
// answer and not a failure of the channel: the connection stands, so the caller
// can gather the inputs and call again over it.
func TestUnfinishedResultLeavesTheConnectionUp(t *testing.T) {
	c, s := discoveryClient(t, emptyToolsFrame(), inputRequiredFrame(map[string]any{"requestState": "opaque"}))

	if _, err := c.CallTool(context.Background(), "x", nil); err == nil {
		t.Fatal("expected an unfinished result")
	}
	if !c.Connected() {
		t.Fatal("the client disconnected over an unfinished result")
	}
	if _, disconnects, _ := s.lifecycle(); disconnects != 0 {
		t.Fatalf("the transport was torn down %d time(s)", disconnects)
	}
}
