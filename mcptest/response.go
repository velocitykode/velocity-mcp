package mcptest

import (
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// Response is the decoded reply to one driven JSON-RPC message plus fluent,
// t-aware assertions. It mirrors laravel/mcp's Server\Testing\TestResponse: each
// Assert* method fails the test (via t.Fatalf) when the expectation does not
// hold and returns the Response so assertions chain.
//
// The assertions read the JSON-RPC result generically, so a single Response type
// serves tool, resource, prompt, and list replies. Methods are named for the
// reply shape they inspect (AssertText for content, AssertToolListed for a
// tools/list reply, ...). A nil reply (a notification, which yields no response)
// is reported by AssertHasResponse / AssertNoResponse.
type Response struct {
	t      testing.TB
	method string
	resp   *jsonrpc.Response
	// result is the decoded "result" object of a success reply, or nil for an
	// error reply or a reply with no result. Decoded once at construction.
	result map[string]any
}

// newResponse builds a Response, decoding the reply's result object once.
func newResponse(t testing.TB, method string, resp *jsonrpc.Response) *Response {
	return &Response{
		t:      t,
		method: method,
		resp:   resp,
		result: decodeResultObject(resp),
	}
}

// Raw returns the underlying decoded JSON-RPC response (nil for a notification
// reply), for assertions the fluent API does not cover.
func (r *Response) Raw() *jsonrpc.Response { return r.resp }

// Result returns the decoded "result" object of a success reply, or nil for an
// error reply or a reply without a result.
func (r *Response) Result() map[string]any { return r.result }

// fatalf reports an assertion failure through t, degrading to a no-op when t is
// nil (a harness built without a *testing.T).
func (r *Response) fatalf(format string, args ...any) {
	if r.t == nil {
		return
	}
	r.t.Helper()
	r.t.Fatalf(format, args...)
}

// AssertOk asserts the reply carries no error: neither a protocol-level error
// object nor a tool-level isError result. Mirrors laravel/mcp's assertOk.
func (r *Response) AssertOk() *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if errs := r.errors(); len(errs) > 0 {
		r.fatalf("mcptest: %s: expected no errors, got: %s", r.method, strings.Join(errs, "; "))
	}
	return r
}

// AssertError asserts the reply carries at least one error (a protocol-level
// error object or a tool-level isError result). When messages are supplied, each
// must appear as a substring of some error message. Mirrors laravel/mcp's
// assertHasErrors.
func (r *Response) AssertError(messages ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	errs := r.errors()
	if len(errs) == 0 {
		r.fatalf("mcptest: %s: expected an error, but the reply has none", r.method)
		return r
	}
	for _, want := range messages {
		if !containsAny(errs, want) {
			r.fatalf("mcptest: %s: expected error containing %q, got: %s", r.method, want, strings.Join(errs, "; "))
		}
	}
	return r
}

// AssertErrorCode asserts the reply is a protocol-level error response with the
// given JSON-RPC error code (e.g. jsonrpc.CodeInvalidParams).
func (r *Response) AssertErrorCode(code int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if r.resp == nil || r.resp.Error == nil {
		r.fatalf("mcptest: %s: expected a protocol error with code %d, got no error", r.method, code)
		return r
	}
	if r.resp.Error.Code != code {
		r.fatalf("mcptest: %s: expected error code %d, got %d (%s)", r.method, code, r.resp.Error.Code, r.resp.Error.Message)
	}
	return r
}

// AssertText asserts the given text appears as a substring of some content
// message in the reply (tool content, prompt message content, or resource
// contents), mirroring laravel/mcp's assertSee. Multiple arguments must all be
// present (each may match a different message).
func (r *Response) AssertText(texts ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	seeable := r.seeable()
	for _, want := range texts {
		if !containsAny(seeable, want) {
			r.fatalf("mcptest: %s: expected to see %q in the reply content, got: %s", r.method, want, strings.Join(seeable, "; "))
		}
	}
	return r
}

// AssertDontSeeText asserts the given text does NOT appear in any content
// message in the reply, mirroring laravel/mcp's assertDontSee.
func (r *Response) AssertDontSeeText(texts ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	seeable := r.seeable()
	for _, unwanted := range texts {
		if containsAny(seeable, unwanted) {
			r.fatalf("mcptest: %s: did not expect to see %q in the reply content", r.method, unwanted)
		}
	}
	return r
}

// AssertHasResponse asserts the message produced a reply (it was a request, not
// a notification).
func (r *Response) AssertHasResponse() *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if r.resp == nil {
		r.fatalf("mcptest: %s: expected a response, but none was produced", r.method)
	}
	return r
}

// AssertNoResponse asserts the message produced no reply (a notification).
func (r *Response) AssertNoResponse() *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if r.resp != nil {
		r.fatalf("mcptest: %s: expected no response, but one was produced", r.method)
	}
	return r
}

// AssertResult asserts the decoded result object contains key bound to want
// (compared as their JSON-normalised forms). It is the generic escape hatch for
// result fields without a dedicated assertion.
func (r *Response) AssertResult(key string, want any) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if r.result == nil {
		r.fatalf("mcptest: %s: expected a result object with key %q, but the reply has no result", r.method, key)
		return r
	}
	got, ok := r.result[key]
	if !ok {
		r.fatalf("mcptest: %s: result is missing key %q", r.method, key)
		return r
	}
	if !jsonEqual(got, want) {
		r.fatalf("mcptest: %s: result[%q] = %#v, want %#v", r.method, key, got, want)
	}
	return r
}

// AssertProtocolVersion asserts an initialize reply negotiated the given
// protocol version.
func (r *Response) AssertProtocolVersion(version string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.AssertResult("protocolVersion", version)
}

// AssertServerName asserts an initialize reply advertises the given server name
// under serverInfo.name.
func (r *Response) AssertServerName(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	info, _ := r.result["serverInfo"].(map[string]any)
	if got, _ := info["name"].(string); got != name {
		r.fatalf("mcptest: %s: serverInfo.name = %q, want %q", r.method, got, name)
	}
	return r
}

// AssertToolListed asserts a tools/list reply includes a tool with the given
// name.
func (r *Response) AssertToolListed(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.listContainsName("tools", name) {
		r.fatalf("mcptest: %s: tool %q was not listed; listed: %s", r.method, name, strings.Join(r.listedNames("tools"), ", "))
	}
	return r
}

// AssertToolNotListed asserts a tools/list reply does NOT include a tool with
// the given name.
func (r *Response) AssertToolNotListed(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if r.listContainsName("tools", name) {
		r.fatalf("mcptest: %s: tool %q was listed but should not have been", r.method, name)
	}
	return r
}

// AssertToolCount asserts a tools/list reply lists exactly n tools.
func (r *Response) AssertToolCount(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if got := len(r.listItems("tools")); got != n {
		r.fatalf("mcptest: %s: listed %d tools, want %d", r.method, got, n)
	}
	return r
}

// AssertResourceListed asserts a resources/list reply includes a resource with
// the given name.
func (r *Response) AssertResourceListed(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.listContainsName("resources", name) {
		r.fatalf("mcptest: %s: resource %q was not listed; listed: %s", r.method, name, strings.Join(r.listedNames("resources"), ", "))
	}
	return r
}

// AssertPromptListed asserts a prompts/list reply includes a prompt with the
// given name.
func (r *Response) AssertPromptListed(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.listContainsName("prompts", name) {
		r.fatalf("mcptest: %s: prompt %q was not listed; listed: %s", r.method, name, strings.Join(r.listedNames("prompts"), ", "))
	}
	return r
}

// errors returns the human-readable error messages for the reply: a
// protocol-level error message, or (when the tool result carries isError:true)
// the tool content messages, mirroring laravel/mcp's TestResponse::errors.
func (r *Response) errors() []string {
	if r.resp != nil && r.resp.Error != nil {
		return []string{r.resp.Error.Message}
	}
	if r.result != nil {
		if isErr, _ := r.result["isError"].(bool); isErr {
			return r.contentMessages()
		}
	}
	return nil
}

// seeable returns every content message in the reply (content, prompt messages,
// resource contents) plus any error messages, mirroring laravel/mcp's
// TestResponse::assertSee source set.
func (r *Response) seeable() []string {
	out := r.contentMessages()
	out = append(out, r.errors()...)
	return dedupeNonEmpty(out)
}

// contentMessages extracts the text/data/blob string of every content item in
// the reply, across the three result shapes (tool content, prompt messages,
// resource contents). It mirrors laravel/mcp's TestResponse::content.
func (r *Response) contentMessages() []string {
	if r.result == nil {
		return nil
	}
	var out []string

	// tools/call: result.content[] -> {text|data}
	for _, item := range asMaps(r.result["content"]) {
		out = append(out, firstString(item, "text", "data"))
	}
	// prompts/get: result.messages[] -> .content -> {text|data}
	for _, msg := range asMaps(r.result["messages"]) {
		if c, ok := msg["content"].(map[string]any); ok {
			out = append(out, firstString(c, "text", "data"))
		}
	}
	// resources/read: result.contents[] -> {text|blob}
	for _, item := range asMaps(r.result["contents"]) {
		out = append(out, firstString(item, "text", "blob"))
	}

	return dedupeNonEmpty(out)
}

// listContainsName reports whether the list under key contains an item whose
// "name" equals name.
func (r *Response) listContainsName(key, name string) bool {
	for _, n := range r.listedNames(key) {
		if n == name {
			return true
		}
	}
	return false
}

// listedNames returns the "name" of every item in the list under key.
func (r *Response) listedNames(key string) []string {
	items := r.listItems(key)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if n, ok := item["name"].(string); ok {
			out = append(out, n)
		}
	}
	return out
}

// listItems returns the list under key in the result object as a slice of maps.
func (r *Response) listItems(key string) []map[string]any {
	if r.result == nil {
		return nil
	}
	return asMaps(r.result[key])
}

// decodeResultObject decodes a success reply's "result" into a map, or nil for
// an error reply, a notification (nil resp), or a non-object result.
func decodeResultObject(resp *jsonrpc.Response) map[string]any {
	if resp == nil || resp.Error != nil || len(resp.Result) == 0 {
		return nil
	}
	var m map[string]any
	if err := jsonUnmarshal(resp.Result, &m); err != nil {
		return nil
	}
	return m
}
