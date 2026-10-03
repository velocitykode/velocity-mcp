package client

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers how a listing follows the cursors the server hands out. The
// specification types the cursor as an opaque string and lets the client read
// one thing off it: whether a non-null value was provided. An empty string is a
// value, so it names a page and must not be read as the end of the results; a
// missing or null member is the end; and a member of any other type is a result
// the specification does not describe.

// listingKinds names the three list requests this client follows, with a way to
// read each whole listing and return the names it carries.
var listingKinds = []struct {
	kind string
	list func(c *Client) ([]string, error)
}{
	{
		kind: "tools",
		list: func(c *Client) ([]string, error) {
			tools, err := c.Tools(context.Background())
			names := make([]string, 0, len(tools))
			for _, tool := range tools {
				names = append(names, tool.Name)
			}
			return names, err
		},
	},
	{
		kind: "resources",
		list: func(c *Client) ([]string, error) {
			resources, err := c.Resources(context.Background())
			names := make([]string, 0, len(resources))
			for _, resource := range resources {
				names = append(names, resource.Name)
			}
			return names, err
		},
	},
	{
		kind: "prompts",
		list: func(c *Client) ([]string, error) {
			prompts, err := c.Prompts(context.Background())
			names := make([]string, 0, len(prompts))
			for _, prompt := range prompts {
				names = append(names, prompt.Name)
			}
			return names, err
		},
	},
}

// listEntry builds one entry of a listing of the given kind, carrying the
// members every kind requires.
func listEntry(kind, name string) map[string]any {
	entry := map[string]any{"name": name}
	if kind == "resources" {
		entry["uri"] = "file:///" + name
	}
	return entry
}

// cursorSent returns the cursor member of a list request's params exactly as it
// went on the wire, or "absent" when the request carried none.
func cursorSent(params json.RawMessage) string {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(params, &members); err != nil {
		return "absent"
	}
	raw, present := members["cursor"]
	if !present {
		return "absent"
	}
	return string(raw)
}

// TestAnEmptyStringCursorNamesAPage asserts the rule the specification spells
// out: an empty string is a valid cursor and must not be treated as the end of
// the results. The page it names is asked for with the cursor sent back exactly
// as it arrived, and its entries are part of the listing, on every list request
// and over both handshakes.
func TestAnEmptyStringCursorNamesAPage(t *testing.T) {
	for _, era := range []string{"initialize", "discovery"} {
		for _, lk := range listingKinds {
			t.Run(era+"/"+lk.kind, func(t *testing.T) {
				var cursors []string
				f := newFakeTransport()
				if era == "discovery" {
					f.on("server/discover", discoverHandler(LatestProtocolVersion))
				}
				f.on(lk.kind+"/list", func(id jsonrpc.ID, params json.RawMessage) *jsonrpc.Response {
					cursors = append(cursors, cursorSent(params))
					var page map[string]any
					switch len(cursors) {
					case 1:
						page = map[string]any{lk.kind: []any{listEntry(lk.kind, "first")}, "nextCursor": ""}
					default:
						page = map[string]any{lk.kind: []any{listEntry(lk.kind, "second")}}
					}
					resp, _ := jsonrpc.NewResult(id, page)
					return resp
				})
				c := newTestClient(f)

				names, err := lk.list(c)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if want := []string{"first", "second"}; !slices.Equal(names, want) {
					t.Fatalf("listed %v, want %v: the page the empty cursor names was dropped", names, want)
				}
				if want := []string{"absent", `""`}; !slices.Equal(cursors, want) {
					t.Fatalf("cursors sent = %v, want %v", cursors, want)
				}
			})
		}
	}
}

// TestACursorIsSentBackExactlyAsItArrived asserts the cursor is opaque: whatever
// the server hands out, whitespace and unicode included, goes back untouched.
func TestACursorIsSentBackExactlyAsItArrived(t *testing.T) {
	for _, cursor := range []string{" ", "eyJwYWdlIjogMn0=", "état ☕ \t", "null", "0"} {
		t.Run(strconv.Quote(cursor), func(t *testing.T) {
			var cursors []string
			f := newFakeTransport()
			f.on("prompts/list", func(id jsonrpc.ID, params json.RawMessage) *jsonrpc.Response {
				cursors = append(cursors, cursorSent(params))
				page := map[string]any{"prompts": []any{listEntry("prompts", "p"+strconv.Itoa(len(cursors)))}}
				if len(cursors) == 1 {
					page["nextCursor"] = cursor
				}
				resp, _ := jsonrpc.NewResult(id, page)
				return resp
			})
			c := newTestClient(f)

			prompts, err := c.Prompts(context.Background())
			if err != nil {
				t.Fatalf("prompts: %v", err)
			}
			if len(prompts) != 2 {
				t.Fatalf("listed %d prompts, want 2", len(prompts))
			}
			quoted, _ := json.Marshal(cursor)
			if want := []string{"absent", string(quoted)}; !slices.Equal(cursors, want) {
				t.Fatalf("cursors sent = %v, want %v", cursors, want)
			}
		})
	}
}

// TestACursorIsSentBackInTheSpellingItArrivedIn asserts the cursor goes back
// byte for byte, not merely with the value a Go string holds of it. A cursor is
// opaque: a spelling the decoder has to replace (a lone surrogate escape), one
// it would normalize (an escaped letter or slash), and a character the standard
// encoder rewrites as an escape all have to reach the server as they left it.
func TestACursorIsSentBackInTheSpellingItArrivedIn(t *testing.T) {
	tests := []struct {
		name string
		// cursor is the nextCursor member as the server writes it.
		cursor string
	}{
		{name: "a lone surrogate escape", cursor: `"a\ud800b"`},
		{name: "an escaped letter", cursor: `"\u0041BC"`},
		{name: "an escaped slash", cursor: `"a\/b"`},
		{name: "an ampersand and angle brackets written plainly", cursor: `"a&b<c>d"`},
		{name: "line and paragraph separators written plainly", cursor: "\"a\u2028b\u2029c\""},
		{name: "the same characters written as escapes", cursor: `"\u0026\u003c\u003e\u2028\u2029"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":{"prompts":[{"name":"p1"}],"nextCursor":` + tt.cursor + `}}`
			second := `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":{"prompts":[{"name":"p2"}]}}`
			s := newScriptedTransport(discoverFrame(LatestProtocolVersion), first, second)
			c := newScriptedClient(s)

			prompts, err := c.Prompts(context.Background())
			if err != nil {
				t.Fatalf("prompts: %v", err)
			}
			if len(prompts) != 2 {
				t.Fatalf("listed %d prompts, want 2", len(prompts))
			}
			if got := cursorSent(json.RawMessage(s.rawMember(t, 2, "params"))); got != tt.cursor {
				t.Fatalf("the cursor went back as %s, want %s byte for byte", got, tt.cursor)
			}
		})
	}
}

// TestAMissingOrNullCursorEndsTheListing asserts the two shapes that end a
// listing: a nextCursor member that is absent, and one that is null. Neither
// asks for another page.
func TestAMissingOrNullCursorEndsTheListing(t *testing.T) {
	for _, lk := range listingKinds {
		for _, shape := range []string{"absent", "null"} {
			t.Run(lk.kind+"/"+shape, func(t *testing.T) {
				pages := 0
				f := newFakeTransport()
				f.on(lk.kind+"/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
					pages++
					page := map[string]any{lk.kind: []any{listEntry(lk.kind, "only")}}
					if shape == "null" {
						page["nextCursor"] = nil
					}
					resp, _ := jsonrpc.NewResult(id, page)
					return resp
				})
				c := newTestClient(f)

				names, err := lk.list(c)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if want := []string{"only"}; !slices.Equal(names, want) {
					t.Fatalf("listed %v, want %v", names, want)
				}
				if pages != 1 {
					t.Fatalf("asked for %d pages, want 1", pages)
				}
			})
		}
	}
}

// TestACursorOfAnotherTypeIsAMalformedResult asserts a nextCursor member that
// is not a string ends nothing: it is reported, so a listing cut short never
// passes for the whole catalogue. The names on the pages that were not read are
// then not taken for names the server does not advertise, which a call by name
// shows by reading the catalogue again rather than being sent on the strength
// of the truncated one.
func TestACursorOfAnotherTypeIsAMalformedResult(t *testing.T) {
	for _, lk := range listingKinds {
		for _, cursor := range []string{"42", "true", "{}", "[]", `{"page":2}`} {
			t.Run(lk.kind+"/"+cursor, func(t *testing.T) {
				frame := `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":{"resultType":"complete","` + lk.kind +
					`":[` + mustJSON(listEntry(lk.kind, "first")) + `],"nextCursor":` + cursor + `,"ttlMs":600000}}`
				c, s := discoveryClient(t, frame, toolsFrame(plainTool("second")), toolCallFrame("done"))

				names, err := lk.list(c)
				want := "invalid " + lk.kind + "/list response from server: the nextCursor member is not a string"
				if err == nil || err.Error() != want {
					t.Fatalf("list = %v, %v; want the error %q", names, err, want)
				}
				if len(names) != 0 {
					t.Fatalf("listed %v from a malformed result", names)
				}

				// Nothing of the truncated listing was recorded as the catalogue:
				// a call by name reads it again.
				if _, err := c.CallTool(context.Background(), "second", nil); err != nil {
					t.Fatalf("call: %v", err)
				}
				wantMethods := []string{"server/discover", lk.kind + "/list", "tools/list", "tools/call"}
				if got := s.methods(); !slices.Equal(got, wantMethods) {
					t.Fatalf("methods = %v, want %v", got, wantMethods)
				}
			})
		}
	}
}

// listingCeiling is how long a listing the client must give up by itself is
// given to do so. A thousand pages over a transport in memory take a fraction
// of a second; a listing still running after this is one nothing bounds.
const listingCeiling = 10 * time.Second

// TestAListingIsBounded asserts a server that hands out a fresh cursor on every
// page is not followed for ever: the listing is given up after maxListPages
// pages with a typed error naming the request and the count, nothing of it is
// returned, and no further page is asked for.
func TestAListingIsBounded(t *testing.T) {
	for _, lk := range listingKinds {
		t.Run(lk.kind, func(t *testing.T) {
			// pages is read by the test only once the listing has returned. ended
			// has the server end the listing, which is how a listing that was
			// not bounded is brought to a stop so the test can report it.
			pages := 0
			var ended atomic.Bool
			f := newFakeTransport()
			f.on(lk.kind+"/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
				pages++
				page := map[string]any{lk.kind: []any{listEntry(lk.kind, "entry-"+strconv.Itoa(pages))}}
				if !ended.Load() {
					page["nextCursor"] = "cursor-" + strconv.Itoa(pages)
				}
				resp, _ := jsonrpc.NewResult(id, page)
				return resp
			})
			c := newTestClient(f)

			type outcome struct {
				names []string
				err   error
			}
			listed := make(chan outcome, 1)
			go func() {
				names, err := lk.list(c)
				listed <- outcome{names, err}
			}()
			var names []string
			var err error
			select {
			case got := <-listed:
				names, err = got.names, got.err
			case <-time.After(listingCeiling):
				ended.Store(true)
				<-listed
				t.Fatalf("the listing was still being followed after %v: %d pages read, with a bound of %d",
					listingCeiling, pages, maxListPages)
			}
			var unbounded *UnboundedListingError
			if !errors.As(err, &unbounded) {
				t.Fatalf("error = %v (%T), want an unbounded listing", err, err)
			}
			if unbounded.Method != lk.kind+"/list" || unbounded.Pages != maxListPages {
				t.Fatalf("error = %+v, want method %s/list after %d pages", unbounded, lk.kind, maxListPages)
			}
			want := "the server kept handing out cursors for [" + lk.kind + "/list] past " +
				strconv.Itoa(maxListPages) + " pages, so the listing was given up"
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}
			if pages != maxListPages {
				t.Fatalf("asked for %d pages, want exactly %d", pages, maxListPages)
			}
			if len(names) != 0 {
				t.Fatalf("listed %d entries of a listing that was given up", len(names))
			}
		})
	}
}

// TestARepeatedCursorIsReportedWithoutEchoingIt asserts a server handing out
// the same cursor twice is stopped at once, and that the cursor, which is the
// server's to choose and may carry anything, is not copied into the error.
func TestARepeatedCursorIsReportedWithoutEchoingIt(t *testing.T) {
	hostile := "\x1b[2J\r\nfake log line"
	f := newFakeTransport()
	pages := 0
	f.on("resources/list", func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		pages++
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"resources":  []any{listEntry("resources", "x")},
			"nextCursor": hostile,
		})
		return resp
	})
	c := newTestClient(f)

	_, err := c.Resources(context.Background())
	if err == nil || err.Error() != "repeated resources/list cursor received from server" {
		t.Fatalf("error = %v", err)
	}
	if pages != 2 {
		t.Fatalf("asked for %d pages, want 2: the repeat is known the moment it is handed out", pages)
	}
}

// mustJSON renders a value as JSON for a hand-written frame.
func mustJSON(value any) string {
	out, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// discoverHandler answers server/discover advertising the given versions, with
// the caching hints the specification requires a server to state on it.
func discoverHandler(versions ...string) fakeHandler {
	return func(id jsonrpc.ID, _ json.RawMessage) *jsonrpc.Response {
		offered := make([]any, 0, len(versions))
		for _, version := range versions {
			offered = append(offered, version)
		}
		resp, _ := jsonrpc.NewResult(id, map[string]any{
			"resultType":        "complete",
			"supportedVersions": offered,
			"capabilities":      map[string]any{},
			"ttlMs":             600000,
			"cacheScope":        "private",
		})
		return resp
	}
}
