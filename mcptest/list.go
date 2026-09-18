package mcptest

import (
	"encoding/json"
	"fmt"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file drives the four MCP list methods to exhaustion. A list reply is one
// page: the server answers with at most a page of items plus a "nextCursor"
// when more remain. A registration assertion that read only the first page would
// report a primitive as not listed merely because it sits on a later page, which
// is the worst failure mode a test helper can have, so every list driver follows
// the cursor and merges the pages before an assertion runs. Server.Send stays
// single-page for tests that want to inspect one page's wire shape.

// ListResourceTemplates requests the resource-template catalogue
// (resources/templates/list), following pagination, and returns the merged reply
// for assertions.
func (s *Server) ListResourceTemplates() *Response {
	if s.t != nil {
		s.t.Helper()
	}
	return s.listAll("resources/templates/list", "resourceTemplates")
}

// listAll drives method until the server stops handing out a cursor and returns
// a Response whose result carries every page's items merged under key.
func (s *Server) listAll(method, key string) *Response {
	if s.t != nil {
		s.t.Helper()
	}
	page, problem := mergeListPages(key, func(cursor string) *Response {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		return s.call(method, params)
	})
	if problem != "" {
		s.fatalf("mcptest: %s: %s", method, problem)
	}
	return page
}

// maxListPages bounds the page walk. A server whose cursor keeps advancing
// would otherwise hand out pages forever and hang the test rather than fail it;
// the repeated-cursor guard below only catches a server that stands still. The
// bound is far beyond any real catalogue (it is 10000 pages, and the smallest
// page a server may serve is one item).
const maxListPages = 10000

// mergeListPages requests page after page through fetch, following each reply's
// nextCursor, and returns the last reply carrying every page's items under key
// (with the spent cursor dropped, since the merged view is the whole
// catalogue). Notifications emitted while any page was handled are carried over.
//
// The second return value describes a server that never finished paginating: it
// re-issued a cursor it had already handed out, it kept issuing fresh ones past
// maxListPages, it answered a list method with something that is not a list, or
// it handed out a cursor that is not a string (see pageCursor).
// It is empty for a well-behaved server, and these guards are what turn such a
// server into a failure rather than a hung or vacuously green test. A reply
// without a result object (an error reply, or a method the server does not
// serve) ends the walk and is returned as it arrived, so the error assertions
// report it and the registration assertions refuse to read it as an empty
// catalogue.
func mergeListPages(key string, fetch func(cursor string) *Response) (*Response, string) {
	var (
		items    []any
		rawList  []json.RawMessage
		notes    []*jsonrpc.Notification
		cursor   string
		issued   = map[string]struct{}{}
		lastPage *Response
	)

	for page := 0; ; page++ {
		lastPage = fetch(cursor)
		notes = append(notes, lastPage.notifications...)
		if lastPage.result == nil {
			lastPage.notifications = notes
			return lastPage, ""
		}

		pageItems, isList := lastPage.result[key].([]any)
		if !isList {
			// The result carries no array under key, so there is no catalogue to
			// merge. The page is returned as it arrived (rather than rewritten
			// with the items gathered so far) so no assertion can mistake it for
			// a complete, empty list.
			lastPage.notifications = notes
			return lastPage, fmt.Sprintf("the reply carries no %q list, so the catalogue cannot be read", key)
		}
		items = append(items, pageItems...)
		if raw, ok := lastPage.rawResultPath(key); ok {
			rawList = append(rawList, rawItems(raw)...)
		}

		next, wellTyped := pageCursor(lastPage.result)
		if !wellTyped {
			raw, _ := lastPage.rawResultPath("nextCursor")
			described := describeJSON(raw)
			return mergedPage(lastPage, key, items, rawList, notes),
				fmt.Sprintf("server answered with a \"nextCursor\" that is not a string (%s), so the catalogue cannot be walked to its end", described)
		}
		if next == "" {
			return mergedPage(lastPage, key, items, rawList, notes), ""
		}
		if _, repeated := issued[next]; repeated {
			return mergedPage(lastPage, key, items, rawList, notes),
				fmt.Sprintf("server repeated pagination cursor %q instead of advancing", next)
		}
		if page+1 >= maxListPages {
			return mergedPage(lastPage, key, items, rawList, notes),
				fmt.Sprintf("server issued more than %d pages without exhausting the catalogue", maxListPages)
		}
		issued[next] = struct{}{}
		cursor = next
	}
}

// pageCursor returns the pagination cursor a list page hands out, and reports
// whether the page states one this walk may follow. A page carrying no
// "nextCursor" (or a JSON null in its place) is the last one and yields ("",
// true). So does one carrying an empty string: it names no page to request, and
// the drivers leave the cursor parameter out for it, so following it would
// re-request the first page rather than advance.
//
// The specification types the field as an optional opaque string token, so a
// page that answers with a number, an array, an object, or a boolean is
// malformed. Reading such a value as an absent cursor would end the walk
// silently and turn half a catalogue into a complete one: a page of
// {"tools": [], "nextCursor": 123} would pass for an exhausted catalogue, and
// every "not listed" and "count is zero" assertion would hold against it. The
// walk reports it instead and stops there, the way it reports a server that
// repeats a cursor or never stops issuing fresh ones. Like those two, and
// unlike the reply that carries no list at all, it hands back the pages
// gathered so far merged (see mergedPage): the report is the finding, and with
// a testing.TB whose Fatalf aborts nothing runs after it.
func pageCursor(result map[string]any) (string, bool) {
	value, present := result["nextCursor"]
	if !present || value == nil {
		return "", true
	}
	next, isString := value.(string)
	if !isString {
		return "", false
	}
	return next, true
}

// mergedPage rewrites the last page's decoded result, its raw members, and the
// reply envelope itself so all three carry the merged item set under key and no
// leftover cursor.
//
// The envelope is rewritten too because Raw is the documented escape hatch for
// assertions the fluent API does not cover. Left as it arrived it would carry
// the last page alone, so a custom assertion reading Raw().Result would miss
// every entry merged from the pages before it, while Result and the fluent
// assertions see the whole catalogue. The three views have to agree.
func mergedPage(page *Response, key string, items []any, rawList []json.RawMessage, notes []*jsonrpc.Notification) *Response {
	page.result[key] = items
	delete(page.result, "nextCursor")
	if page.raw != nil {
		page.raw[key] = joinRawArray(rawList)
		delete(page.raw, "nextCursor")
		// The members are already wire bytes, so re-encoding them cannot fail for
		// any reply that reached this far; a reply that somehow defeats it keeps
		// the envelope it arrived with rather than losing it.
		if encoded, err := encodeJSON(page.raw); err == nil && page.resp != nil {
			page.resp.Result = encoded
		}
	}
	page.notifications = notes
	return page
}
