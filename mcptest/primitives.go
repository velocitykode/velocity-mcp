package mcptest

import (
	"strconv"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file completes the registration assertions over the four MCP list
// methods (tools/list, resources/list, resources/templates/list, prompts/list)
// and adds the per-entry assertions for a listed primitive's descriptive fields.
// The list drivers themselves live in list.go, which follows pagination so these
// assertions see the whole catalogue rather than its first page.
//
// Naming follows the package's existing pair, AssertToolListed /
// AssertToolNotListed: "listed" is this package's word for "registered and
// advertised to the client". The complementary check, that invoking an
// unregistered primitive is reported as not found, is AssertToolNotRegistered
// and friends below.

// AssertResourceNotListed asserts a resources/list reply does NOT include a
// resource with any of the given names. Called with no names it asserts only
// that the reply carries a resource list.
func (r *Response) AssertResourceNotListed(names ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertNotListed("resources", "resource", names)
}

// AssertPromptNotListed asserts a prompts/list reply does NOT include a prompt
// with any of the given names. Called with no names it asserts only that the
// reply carries a prompt list.
func (r *Response) AssertPromptNotListed(names ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertNotListed("prompts", "prompt", names)
}

// AssertResourceTemplateListed asserts a resources/templates/list reply includes
// a template with each of the given names. Called with no names it asserts only
// that the reply carries a resource template list.
func (r *Response) AssertResourceTemplateListed(names ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertListed("resourceTemplates", "resource template", names)
}

// AssertResourceTemplateNotListed asserts a resources/templates/list reply does
// NOT include a template with any of the given names. Called with no names it
// asserts only that the reply carries a resource template list.
func (r *Response) AssertResourceTemplateNotListed(names ...string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertNotListed("resourceTemplates", "resource template", names)
}

// AssertResourceCount asserts a resources/list reply lists exactly n resources.
func (r *Response) AssertResourceCount(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertListCount("resources", "resources", n)
}

// AssertPromptCount asserts a prompts/list reply lists exactly n prompts.
func (r *Response) AssertPromptCount(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertListCount("prompts", "prompts", n)
}

// AssertResourceTemplateCount asserts a resources/templates/list reply lists
// exactly n templates.
func (r *Response) AssertResourceTemplateCount(n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertListCount("resourceTemplates", "resource templates", n)
}

// hasList reports whether the reply's result carries an array under key.
func (r *Response) hasList(key string) bool {
	if r.result == nil {
		return false
	}
	_, ok := r.result[key].([]any)
	return ok
}

// requireList fails the test unless the reply carries an array of objects under
// key, and reports whether the caller may go on to read it.
//
// Every registration assertion goes through it, because without it the negative
// ones hold on a reply that lists nothing at all: an error reply (the shape a
// missing blank import of server/methods gives every list method), a reply to
// another method entirely, or a page the server answered with a non-list. A
// "not listed" or a "count is zero" that passes against such a reply reports a
// registered primitive as absent, which is the worst failure mode a test helper
// can have.
//
// The elements are checked here too, before anything counts or searches them.
// Reading a list drops whatever it cannot make a named primitive of, so a page
// of `[42, null]` or of `[{}]` would otherwise count as an empty catalogue and
// satisfy every "not listed" assertion there is.
func (r *Response) requireList(key string) bool {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.hasList(key) {
		r.fatalf("mcptest: %s: expected the reply to carry a %q list, but it carries none; errors: %s",
			r.method, key, describeValues(r.errors()))
		return false
	}
	if index, raw, fault := r.firstMalformedEntry(key); fault != "" {
		r.fatalf("mcptest: %s: the %q list holds an entry that %s at index %d: %s",
			r.method, key, fault, index, raw)
		return false
	}
	return true
}

// firstMalformedEntry reports the index, rendering, and fault of the first entry
// of the list under key that the registration assertions cannot read, and an
// empty fault when every entry is readable. The decoded view is what is walked,
// because that is what the assertions read; the wire bytes are used for the
// message when they are at hand.
//
// A readable entry is an object carrying a string "name": the specification
// makes name a required member of a tool, a resource, a resource template, and a
// prompt alike, and it is the member every registration assertion matches on. An
// entry without one is a primitive those assertions are blind to, so it is
// reported here rather than dropped.
func (r *Response) firstMalformedEntry(key string) (int, string, string) {
	entries, _ := r.result[key].([]any)
	raw := rawItems(r.rawList(key))
	for index, entry := range entries {
		fault := ""
		object, isObject := entry.(map[string]any)
		if !isObject {
			fault = "is not an object"
		} else if _, named := object["name"].(string); !named {
			fault = `carries no string "name"`
		}
		if fault == "" {
			continue
		}
		if index < len(raw) {
			return index, describeJSON(raw[index]), fault
		}
		return index, jsonString(entry), fault
	}
	return 0, "", ""
}

// rawList returns the wire bytes of the list under key, or nil when the reply
// carries no raw view of it.
func (r *Response) rawList(key string) []byte {
	raw, ok := r.rawResultPath(key)
	if !ok {
		return nil
	}
	return raw
}

// assertListed asserts every name appears in the list under key. kind names the
// primitive in the failure message.
func (r *Response) assertListed(key, kind string, names []string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.requireList(key) {
		return r
	}
	for _, name := range names {
		if !r.listContainsName(key, name) {
			r.fatalf("mcptest: %s: %s %q was not listed; listed: %s",
				r.method, kind, name, describeValues(r.listedNames(key)))
			return r
		}
	}
	return r
}

// assertNotListed asserts no name appears in the list under key.
func (r *Response) assertNotListed(key, kind string, names []string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.requireList(key) {
		return r
	}
	for _, name := range names {
		if r.listContainsName(key, name) {
			r.fatalf("mcptest: %s: %s %q was listed but should not have been; listed: %s",
				r.method, kind, name, describeValues(r.listedNames(key)))
			return r
		}
	}
	return r
}

// assertListCount asserts the list under key holds exactly n items. plural names
// the primitive set in the failure message.
func (r *Response) assertListCount(key, plural string, n int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.requireList(key) {
		return r
	}
	if got := len(r.listItems(key)); got != n {
		r.fatalf("mcptest: %s: listed %d %s, want %d; listed: %s",
			r.method, got, plural, n, describeValues(r.listedNames(key)))
	}
	return r
}

// AssertToolNotRegistered asserts this reply is the protocol-level "not found"
// error the server returns for a tools/call naming an unregistered tool. It is
// the assertion for a call driven against a primitive that was deliberately
// left out of the server, where the reply carries no content to inspect. A tool
// error result does not satisfy it, however its text reads: see
// assertNotRegistered.
func (r *Response) AssertToolNotRegistered(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertNotRegistered("tool", "Tool ["+name+"] not found.", notRegisteredCode)
}

// AssertPromptNotRegistered asserts this reply is the protocol-level "not
// found" error the server returns for a prompts/get (or a completion
// referencing a prompt) naming an unregistered prompt.
func (r *Response) AssertPromptNotRegistered(name string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertNotRegistered("prompt", "Prompt ["+name+"] not found.", notRegisteredCode)
}

// AssertResourceNotRegistered asserts this reply is the protocol-level "not
// found" error the server returns for a resources/read (or a completion
// referencing a resource) naming an unregistered uri. Resources are identified
// by uri, not by name.
//
// Either code the specification gives an unresolvable uri satisfies it, because
// which one the server writes follows the revision the request declares (see
// resourceNotRegisteredCodes). The assertion is about the primitive being
// absent, not about the revision the harness drove, so a suite asserting it
// reads the same either way.
func (r *Response) AssertResourceNotRegistered(uri string) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	return r.assertNotRegistered("resource", "Resource ["+uri+"] not found.", resourceNotRegisteredCodes()...)
}

// notRegisteredCode is the JSON-RPC error code a request naming a primitive the
// server does not have is answered with. The MCP specification reports an
// unknown tool name as an Invalid params error, and the server answers a
// prompts/get and a completion reference to a missing primitive the same way.
const notRegisteredCode = jsonrpc.CodeInvalidParams

// resourceNotRegisteredCodes lists the codes an unresolvable resources/read uri
// is reported with. Revision 2026-07-28 reports it as Invalid params; the
// revisions before it report Resource not found, which is what their clients
// read to tell a resource that is not there from parameters they got wrong.
func resourceNotRegisteredCodes() []int {
	return []int{notRegisteredCode, jsonrpc.CodeResourceNotFound}
}

// assertNotRegistered asserts the reply is the protocol-level not-found error
// for a missing primitive: an error object carrying one of codes whose message
// contains want. kind names the primitive in the failure message.
//
// Only the error object counts. The specification separates a protocol error,
// which reports that the request could not be served at all, from a tool error
// result ("isError": true), which reports that a registered handler ran and
// failed; the second is the opposite claim to the one this assertion makes, so
// a registered handler answering with the text "Tool [ghost] not found." must
// not satisfy AssertToolNotRegistered("ghost"). Reading the reply through
// errors(), which merges both shapes, would let it.
func (r *Response) assertNotRegistered(kind, want string, codes ...int) *Response {
	if r.t != nil {
		r.t.Helper()
	}
	if r.resp == nil || r.resp.Error == nil {
		r.fatalf("mcptest: %s: expected the %s to be unregistered (protocol error %s %q), but the reply carries no protocol error; errors: %s",
			r.method, kind, describeCodes(codes), want, describeValues(r.errors()))
		return r
	}
	if !containsCode(codes, r.resp.Error.Code) {
		r.fatalf("mcptest: %s: expected the %s to be unregistered (protocol error %s %q), got error code %d: %s",
			r.method, kind, describeCodes(codes), want, r.resp.Error.Code, r.resp.Error.Message)
		return r
	}
	if !strings.Contains(r.resp.Error.Message, want) {
		r.fatalf("mcptest: %s: expected the %s to be unregistered (error %q), got: %s",
			r.method, kind, want, r.resp.Error.Message)
	}
	return r
}

// containsCode reports whether codes names code.
func containsCode(codes []int, code int) bool {
	for _, item := range codes {
		if item == code {
			return true
		}
	}
	return false
}

// describeCodes renders the codes a failure message names, so a reader sees
// every code that would have satisfied the assertion rather than one of them.
func describeCodes(codes []int) string {
	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, strconv.Itoa(code))
	}
	return strings.Join(parts, " or ")
}

// Listed is one entry of a list reply (a tool, resource, resource template, or
// prompt) with assertions on its descriptive fields. Obtain one from a list
// reply with Response.Tool, Response.Prompt, Response.Resource, or
// Response.ResourceTemplate; a missing entry fails the test at lookup time and
// yields a Listed whose assertions are no-ops, so a chain reports one failure
// rather than a cascade.
type Listed struct {
	t      testing.TB
	method string
	kind   string
	name   string
	item   map[string]any
}

// Tool returns the tools/list entry with the given name, failing the test when
// no such tool was listed.
func (r *Response) Tool(name string) *Listed {
	if r.t != nil {
		r.t.Helper()
	}
	return r.listed("tools", "tool", name)
}

// Prompt returns the prompts/list entry with the given name, failing the test
// when no such prompt was listed.
func (r *Response) Prompt(name string) *Listed {
	if r.t != nil {
		r.t.Helper()
	}
	return r.listed("prompts", "prompt", name)
}

// Resource returns the resources/list entry with the given name, failing the
// test when no such resource was listed.
func (r *Response) Resource(name string) *Listed {
	if r.t != nil {
		r.t.Helper()
	}
	return r.listed("resources", "resource", name)
}

// ResourceTemplate returns the resources/templates/list entry with the given
// name, failing the test when no such template was listed.
func (r *Response) ResourceTemplate(name string) *Listed {
	if r.t != nil {
		r.t.Helper()
	}
	return r.listed("resourceTemplates", "resource template", name)
}

// listed finds the entry named name in the list under key.
func (r *Response) listed(key, kind, name string) *Listed {
	if r.t != nil {
		r.t.Helper()
	}
	if !r.requireList(key) {
		return &Listed{t: r.t, method: r.method, kind: kind, name: name}
	}
	for _, item := range r.listItems(key) {
		if n, _ := item["name"].(string); n == name {
			return &Listed{t: r.t, method: r.method, kind: kind, name: name, item: item}
		}
	}
	r.fatalf("mcptest: %s: %s %q was not listed; listed: %s",
		r.method, kind, name, describeValues(r.listedNames(key)))
	return &Listed{t: r.t, method: r.method, kind: kind, name: name}
}

// Raw returns the entry's decoded wire object, for assertions the fluent API
// does not cover. It is nil when the entry was not found. The map is the
// reply's own decoded view, not a copy: read it, do not mutate it, or the
// assertions that follow will read what the mutation left behind.
func (l *Listed) Raw() map[string]any { return l.item }

// fatalf reports an assertion failure through t, degrading to a no-op when t is
// nil (a harness built without a *testing.T).
func (l *Listed) fatalf(format string, args ...any) {
	if l.t == nil {
		return
	}
	l.t.Helper()
	l.t.Fatalf(format, args...)
}

// AssertName asserts the entry's advertised name equals want.
func (l *Listed) AssertName(want string) *Listed {
	if l.t != nil {
		l.t.Helper()
	}
	return l.assertField("name", want)
}

// AssertTitle asserts the entry's advertised title equals want. Every list entry
// carries a title: a primitive that declares none is advertised with a headline
// derived from its name.
func (l *Listed) AssertTitle(want string) *Listed {
	if l.t != nil {
		l.t.Helper()
	}
	return l.assertField("title", want)
}

// AssertDescription asserts the entry's advertised description equals want.
func (l *Listed) AssertDescription(want string) *Listed {
	if l.t != nil {
		l.t.Helper()
	}
	return l.assertField("description", want)
}

// assertField asserts the entry's string-valued field equals want.
func (l *Listed) assertField(field, want string) *Listed {
	if l.t != nil {
		l.t.Helper()
	}
	if l.item == nil {
		// The lookup already reported that the entry is missing; asserting on
		// its fields adds nothing, so a chain reports one failure rather than a
		// cascade of them.
		return l
	}
	got, ok := l.item[field].(string)
	if !ok {
		l.fatalf("mcptest: %s: %s %q has no string %s; it advertises: %s",
			l.method, l.kind, l.name, field, jsonString(l.item))
		return l
	}
	if got != want {
		l.fatalf("mcptest: %s: %s %q %s = %q, want %q",
			l.method, l.kind, l.name, field, got, want)
	}
	return l
}
