package mcptest

import (
	"slices"
	"strconv"
	"strings"
)

// This file adds the completion/complete driver and its assertions. A client
// asks the server to complete one argument of a prompt or a resource template,
// naming the primitive with a reference object (ref/prompt or ref/resource) and
// optionally passing the sibling arguments it has already resolved.

// CompletePrompt drives completion/complete for a prompt argument and returns
// the reply for assertions. argument is the prompt argument being completed and
// value the partial text typed so far (which may be empty). resolved carries the
// sibling arguments the client already knows, and may be nil.
func (s *Server) CompletePrompt(name, argument, value string, resolved map[string]string) *Response {
	if s.t != nil {
		s.t.Helper()
	}
	return s.complete(map[string]any{"type": "ref/prompt", "name": name}, argument, value, resolved)
}

// CompleteResource drives completion/complete for a resource URI-template
// variable and returns the reply for assertions. uri is the resource uri or uri
// template, argument the template variable being completed, and value the
// partial text typed so far. resolved carries the sibling variables the client
// already knows, and may be nil.
func (s *Server) CompleteResource(uri, argument, value string, resolved map[string]string) *Response {
	if s.t != nil {
		s.t.Helper()
	}
	return s.complete(map[string]any{"type": "ref/resource", "uri": uri}, argument, value, resolved)
}

// complete sends one completion/complete request for the given reference. The
// context object is only included when the caller supplied resolved arguments,
// so a plain completion sends the minimal params a client would.
func (s *Server) complete(ref map[string]any, argument, value string, resolved map[string]string) *Response {
	if s.t != nil {
		s.t.Helper()
	}
	params := map[string]any{
		"ref": ref,
		"argument": map[string]any{
			"name":  argument,
			"value": value,
		},
	}
	if len(resolved) > 0 {
		args := make(map[string]any, len(resolved))
		for k, v := range resolved {
			args[k] = v
		}
		params["context"] = map[string]any{"arguments": args}
	}
	return s.call("completion/complete", params)
}

// CompletionValues returns the candidate values of a completion/complete reply,
// or an empty slice when the reply carries no completion or one whose [values]
// member is not an array of strings. The assertions below report that shape
// rather than comparing against what could be read out of it: a candidate list
// the specification does not permit must fail a test, not shrink quietly to the
// entries that happen to be strings.
func (r *Response) CompletionValues() []string {
	values, _ := r.completionCandidates()
	if values == nil {
		return []string{}
	}
	return values
}

// completionCandidates reads the completion's candidate values, returning a
// description of the offending shape when the reply's [values] member is
// absent, is not an array, or carries an element that is not a string. The
// member is required of every completion result, so each of those is a reply no
// assertion may hold against.
func (r *Response) completionCandidates() ([]string, string) {
	raw, present := r.rawResultPath("completion", "values")
	if !present {
		return nil, "the completion carries no [values] member"
	}
	items := rawItems(raw)
	if items == nil {
		return nil, "the completion [values] member is not an array: " + describeJSON(raw)
	}
	values := make([]string, 0, len(items))
	for index, item := range items {
		value, ok := rawString(item)
		if !ok {
			return nil, "the completion [values] member holds a candidate that is not a string at index " +
				strconv.Itoa(index) + ": " + describeJSON(item)
		}
		values = append(values, value)
	}
	return values, ""
}

// requireCompletionValues reads the candidate values, failing the test when the
// reply carries no completion or a malformed candidate list, and reporting
// whether the caller may go on to compare them.
func (r *Response) requireCompletionValues(context string) ([]string, bool) {
	if r.t != nil {
		r.t.Helper()
	}
	if r.completion() == nil {
		r.fatalf("mcptest: %s: %s, but the reply carries no completion", r.method, context)
		return nil, false
	}
	values, problem := r.completionCandidates()
	if problem != "" {
		r.fatalf("mcptest: %s: %s, but %s", r.method, context, problem)
		return nil, false
	}
	return values, true
}

// completion returns the reply's completion object, or nil when the reply
// carries none (an error reply, or a different method's reply).
func (r *Response) completion() map[string]any {
	if r.result == nil {
		return nil
	}
	comp, ok := r.result["completion"].(map[string]any)
	if !ok {
		return nil
	}
	return comp
}

// AssertHasCompletions asserts the reply carries a completion object and that
// each of the given values appears among its candidates. Called with no values
// it asserts only that a completion object is present.
func (r *Response) AssertHasCompletions(values ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	got, ok := r.requireCompletionValues("expected a completion in the reply")
	if !ok {
		return r
	}
	for _, want := range values {
		if !slices.Contains(got, want) {
			r.fatalf("mcptest: %s: expected completion value %q, got: %s",
				r.method, want, describeValues(got))
			return r
		}
	}
	return r
}

// AssertCompletionValues asserts the reply's completion candidates are exactly
// the given values, in the given order.
func (r *Response) AssertCompletionValues(values ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	got, ok := r.requireCompletionValues("expected completion values " + describeValues(values))
	if !ok {
		return r
	}
	if !slices.Equal(got, values) {
		r.fatalf("mcptest: %s: completion values = %s, want %s",
			r.method, describeValues(got), describeValues(values))
	}
	return r
}

// AssertCompletionCount asserts the reply's completion carries exactly n
// candidate values.
func (r *Response) AssertCompletionCount(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	got, ok := r.requireCompletionValues("expected " + strconv.Itoa(n) + " completion values")
	if !ok {
		return r
	}
	if len(got) != n {
		r.fatalf("mcptest: %s: completion carries %d values, want %d; got: %s",
			r.method, len(got), n, describeValues(got))
	}
	return r
}

// AssertCompletionTotal asserts the reply's completion advertises n total
// matches. The total may exceed the number of returned values when the server
// truncated the candidate set.
func (r *Response) AssertCompletionTotal(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	comp := r.completion()
	if comp == nil {
		r.fatalf("mcptest: %s: expected a completion total of %d, but the reply carries no completion", r.method, n)
		return r
	}
	total, _ := r.rawResultPath("completion", "total")
	if !jsonEqual(total, n) {
		r.fatalf("mcptest: %s: completion total = %s, want %d", r.method, describeJSON(total), n)
	}
	return r
}

// AssertCompletionHasMore asserts the reply's completion reports the given
// hasMore flag, which tells the client whether the candidate set was truncated.
// The flag is optional on the wire and its absence means false; a hasMore that
// is present but not a boolean is reported as the malformed shape it is rather
// than read as false.
func (r *Response) AssertCompletionHasMore(want bool) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	comp := r.completion()
	if comp == nil {
		r.fatalf("mcptest: %s: expected a completion with hasMore=%t, but the reply carries no completion", r.method, want)
		return r
	}
	flag, present := comp["hasMore"]
	got, isBool := flag.(bool)
	if present && !isBool {
		r.fatalf("mcptest: %s: completion hasMore is not a boolean: %s, want %t",
			r.method, jsonString(flag), want)
		return r
	}
	if got != want {
		r.fatalf("mcptest: %s: completion hasMore = %t, want %t", r.method, got, want)
	}
	return r
}

// describeValues renders a value list for a failure message, naming the empty
// case explicitly.
func describeValues(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	return "[" + strings.Join(values, ", ") + "]"
}
