package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// This file covers the caching hints: how long the client keeps what a server
// stated, and what it does once that has run out. The expectations are the
// specification's. A result is fresh while the local time is before the time it
// was received plus its ttlMs; an absent or negative ttlMs is read as zero, and
// zero is stale the moment it arrives; a stale result is read again the next
// time it is needed; and every page of a listing is dated on its own.

// steppedClock is a local clock a test moves by hand, so a lifetime runs out
// when the test says so rather than when enough of the real one has passed.
type steppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSteppedClock() *steppedClock {
	return &steppedClock{now: time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)}
}

func (k *steppedClock) read() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.now
}

func (k *steppedClock) advance(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.now = k.now.Add(d)
}

// attach makes the clock the one the client measures freshness on. It is called
// before the client sends anything.
func (k *steppedClock) attach(c *Client) { c.proto.now = k.read }

// region is the argument bag the calls below carry: the property the annotated
// definition asks to be mirrored.
func region() map[string]any { return map[string]any{"region": "us-west1"} }

// TestADefinitionIsKeptForTheLifetimeTheServerGaveIt asserts a definition
// settles the headers of a call for exactly as long as the server said it may
// be kept. The server changes the definition behind the client's back, so which
// definition the second call mirrors says whether the first was read again.
func TestADefinitionIsKeptForTheLifetimeTheServerGaveIt(t *testing.T) {
	const year = 365 * 24 * time.Hour

	tests := []struct {
		name string
		// lifetime is the ttlMs member of the first listing, written as given.
		lifetime any
		elapsed  time.Duration
		reread   bool
	}{
		{name: "a millisecond short of the lifetime is fresh", lifetime: 60000, elapsed: 59999 * time.Millisecond},
		{name: "the lifetime itself is stale", lifetime: 60000, elapsed: 60000 * time.Millisecond, reread: true},
		{name: "past the lifetime is stale", lifetime: 60000, elapsed: 60001 * time.Millisecond, reread: true},
		{name: "one millisecond is kept until it has passed", lifetime: 1, elapsed: 0},
		{name: "one millisecond has run out once it has passed", lifetime: 1, elapsed: time.Millisecond, reread: true},
		{name: "no lifetime stated is none", lifetime: nil, elapsed: 0, reread: true},
		{name: "a lifetime of zero is stale on arrival", lifetime: 0, elapsed: 0, reread: true},
		{name: "a negative lifetime is read as zero", lifetime: -60000, elapsed: 0, reread: true},
		{name: "a null lifetime is none", lifetime: json.RawMessage(`null`), elapsed: 0, reread: true},
		{name: "a lifetime written as a string is not one", lifetime: "60000", elapsed: 0, reread: true},
		{name: "a lifetime written as a boolean is not one", lifetime: true, elapsed: 0, reread: true},
		{name: "a fraction of a millisecond is not a whole number of them", lifetime: json.RawMessage(`1.5`), elapsed: 0, reread: true},
		{name: "a whole number written with an exponent is read as it is", lifetime: json.RawMessage(`6e4`), elapsed: 59999 * time.Millisecond},
		{name: "a whole number written with an exponent runs out as it does", lifetime: json.RawMessage(`6e4`), elapsed: 60000 * time.Millisecond, reread: true},
		{name: "a whole number written with a fraction of zero is read as it is", lifetime: json.RawMessage(`60000.0`), elapsed: 59999 * time.Millisecond},
		{name: "the longest lifetime an integer states does not wrap round", lifetime: json.RawMessage(`9223372036854775807`), elapsed: 250 * year},
		{name: "a lifetime beyond what an integer holds is not kept", lifetime: json.RawMessage(`1e30`), elapsed: 0, reread: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frames := []string{
				toolsFrameKeptFor(tc.lifetime, plainTool("execute_sql")),
				toolCallFrame("first"),
			}
			if tc.reread {
				frames = append(frames, toolsFrame(annotatedTool("Region")))
			}
			frames = append(frames, toolCallFrame("second"))

			c, s := discoveryClient(t, frames...)
			clock := newSteppedClock()
			clock.attach(c)

			for _, want := range []string{"first", "second"} {
				result, err := c.CallTool(context.Background(), "execute_sql", region())
				if err != nil {
					t.Fatalf("call: %v", err)
				}
				if result.Text() != want {
					t.Fatalf("text = %q, want %q", result.Text(), want)
				}
				clock.advance(tc.elapsed)
			}

			want := []string{"server/discover", "tools/list", "tools/call", "tools/call"}
			if tc.reread {
				want = []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
			}
			if got := s.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v", got, want)
			}
			if _, present := s.headersAt(2)["Mcp-Param-Region"]; present {
				t.Fatalf("the first call mirrored a header its definition does not ask for: %v", s.headersAt(2))
			}
			last := s.headersAt(len(want) - 1)
			if got, present := last["Mcp-Param-Region"]; tc.reread && got != "us-west1" {
				t.Fatalf("Mcp-Param-Region = %q, want us-west1: the call mirrored a definition that had run out", got)
			} else if !tc.reread && present {
				t.Fatalf("Mcp-Param-Region = %q, want none: the definition in hand was still fresh", got)
			}
		})
	}
}

// TestAToolValueIsReadAgainOnceItsDefinitionHasRunOut asserts the lifetime
// travels with the tool value a listing returned. A caller may hold the value
// for as long as it likes, and calling it past the lifetime of the page that
// carried it reads the definition again before any header is rendered.
func TestAToolValueIsReadAgainOnceItsDefinitionHasRunOut(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrameKeptFor(60000, plainTool("execute_sql")),
		toolCallFrame("first"),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("second"),
	)
	clock := newSteppedClock()
	clock.attach(c)

	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	clock.advance(59999 * time.Millisecond)
	if _, err := tools[0].Call(context.Background(), region()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	clock.advance(time.Millisecond)
	result, err := tools[0].Call(context.Background(), region())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result.Text() != "second" {
		t.Fatalf("text = %q, want second", result.Text())
	}

	want := []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if _, present := s.headersAt(2)["Mcp-Param-Region"]; present {
		t.Fatalf("the fresh definition mirrored a header it does not ask for: %v", s.headersAt(2))
	}
	if got := s.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestADefinitionWithNoLifetimeIsReadOnceForEveryCall asserts a server that
// lets nothing be kept is asked before every call and only once for each. The
// definition a call reads for itself is as current as the server can state it,
// so it is not refused for having no lifetime, which would never end.
func TestADefinitionWithNoLifetimeIsReadOnceForEveryCall(t *testing.T) {
	t.Run("a call by name", func(t *testing.T) {
		c, s := discoveryClient(t,
			toolsFrameKeptFor(0, annotatedTool("Region")),
			toolCallFrame("first"),
			toolsFrameKeptFor(0, annotatedTool("Region")),
			toolCallFrame("second"),
		)
		newSteppedClock().attach(c)

		for range 2 {
			if _, err := c.CallTool(context.Background(), "execute_sql", region()); err != nil {
				t.Fatalf("call: %v", err)
			}
		}
		want := []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
		if got := s.methods(); !slices.Equal(got, want) {
			t.Fatalf("methods = %v, want %v", got, want)
		}
		for _, index := range []int{2, 4} {
			if got := s.headersAt(index)["Mcp-Param-Region"]; got != "us-west1" {
				t.Fatalf("frame %d Mcp-Param-Region = %q, want us-west1", index, got)
			}
		}
	})

	t.Run("a call on a tool value", func(t *testing.T) {
		c, s := discoveryClient(t,
			toolsFrameKeptFor(0, plainTool("execute_sql")),
			toolsFrameKeptFor(0, annotatedTool("Region")),
			toolCallFrame("done"),
		)
		newSteppedClock().attach(c)

		tools, err := c.Tools(context.Background())
		if err != nil {
			t.Fatalf("tools: %v", err)
		}
		if _, err := tools[0].Call(context.Background(), region()); err != nil {
			t.Fatalf("call: %v", err)
		}
		want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
		if got := s.methods(); !slices.Equal(got, want) {
			t.Fatalf("methods = %v, want %v", got, want)
		}
		if got := s.headersAt(3)["Mcp-Param-Region"]; got != "us-west1" {
			t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
		}
	})
}

// pagedToolsFrame builds one page of a tools/list result stating its own
// lifetime, with the cursor of the page after it when there is one.
func pagedToolsFrame(lifetime any, next string, tools ...map[string]any) string {
	entries := make([]any, 0, len(tools))
	for _, tool := range tools {
		entries = append(entries, tool)
	}
	result := map[string]any{"resultType": "complete", "tools": entries, "ttlMs": lifetime, "cacheScope": "private"}
	if next != "" {
		result["nextCursor"] = next
	}
	return resultFrame(result)
}

// TestEveryPageOfAListingIsDatedOnItsOwn asserts the specification's rule for a
// paginated listing: each page states its own lifetime, and what a page stated
// is kept for that long and no longer. What the listing states as a whole, that
// a name it does not carry is one the server does not advertise, rests on every
// page of it, so it lasts as long as the page that runs out first, whichever
// page of the listing that is.
func TestEveryPageOfAListingIsDatedOnItsOwn(t *testing.T) {
	const short, long = 1000, 60000

	layouts := []struct {
		name  string
		pages func() []string
	}{
		{
			name: "the short page comes first",
			pages: func() []string {
				return []string{
					pagedToolsFrame(short, "page-2", plainTool("short_lived")),
					pagedToolsFrame(long, "", plainTool("long_lived")),
				}
			},
		},
		{
			name: "the short page comes last",
			pages: func() []string {
				return []string{
					pagedToolsFrame(long, "page-2", plainTool("long_lived")),
					pagedToolsFrame(short, "", plainTool("short_lived")),
				}
			},
		},
	}
	tests := []struct {
		name    string
		elapsed time.Duration
		call    string
		reread  bool
	}{
		{name: "a tool of the short page while it is fresh", elapsed: 999 * time.Millisecond, call: "short_lived"},
		{name: "a tool of the short page once it has run out", elapsed: 1000 * time.Millisecond, call: "short_lived", reread: true},
		{name: "a tool of the long page once the short one has run out", elapsed: 1000 * time.Millisecond, call: "long_lived"},
		{name: "a tool of the long page once it has run out too", elapsed: 60000 * time.Millisecond, call: "long_lived", reread: true},
		{name: "a name no page carries while every page is fresh", elapsed: 999 * time.Millisecond, call: "unlisted"},
		{name: "a name no page carries once one page has run out", elapsed: 1000 * time.Millisecond, call: "unlisted", reread: true},
	}

	for _, layout := range layouts {
		for _, tc := range tests {
			t.Run(layout.name+"/"+tc.name, func(t *testing.T) {
				frames := layout.pages()
				if tc.reread {
					frames = append(frames, layout.pages()...)
				}
				frames = append(frames, toolCallFrame("done"))

				c, s := discoveryClient(t, frames...)
				clock := newSteppedClock()
				clock.attach(c)

				if _, err := c.Tools(context.Background()); err != nil {
					t.Fatalf("tools: %v", err)
				}
				clock.advance(tc.elapsed)
				if _, err := c.CallTool(context.Background(), tc.call, region()); err != nil {
					t.Fatalf("call: %v", err)
				}

				want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
				if tc.reread {
					want = []string{"server/discover", "tools/list", "tools/list", "tools/list", "tools/list", "tools/call"}
				}
				if got := s.methods(); !slices.Equal(got, want) {
					t.Fatalf("methods = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestACallWhoseDefinitionRanOutIsStillMadeWhenNoneIsToBeHad asserts a call
// refused only because its definition had gone out of date is not left refused
// when the read that follows has no definition to give it. Nothing about the
// call itself was wrong, so it is made with what the caller stated and the
// server's own answer is what the caller is told, exactly as for a call by name.
func TestACallWhoseDefinitionRanOutIsStillMadeWhenNoneIsToBeHad(t *testing.T) {
	tests := []struct {
		name string
		// reread answers the listing the stale definition sends the client for.
		reread string
		// answer is what the server says to the call.
		answer  string
		wantErr string
	}{
		{
			name:    "the server no longer advertises the tool",
			reread:  toolsFrame(),
			answer:  errorFrame(jsonrpc.CodeInvalidParams, "Unknown tool: execute_sql", nil),
			wantErr: "Unknown tool: execute_sql",
		},
		{
			name:   "the server refuses the listing on protocol terms",
			reread: methodNotFoundFrame(),
			answer: toolCallFrame("done"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t, toolsFrameKeptFor(1000, annotatedTool("Region")), tc.reread, tc.answer)
			clock := newSteppedClock()
			clock.attach(c)

			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			clock.advance(time.Second)

			result, err := tools[0].Call(context.Background(), region())
			switch {
			case tc.wantErr != "":
				var rpcErr *jsonrpc.Error
				if !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, tc.wantErr) {
					t.Fatalf("error = %v, want the server's own answer %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("call: %v", err)
			case result.Text() != "done":
				t.Fatalf("text = %q, want done", result.Text())
			}

			want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
			if got := s.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v", got, want)
			}
			if _, present := s.headersAt(3)["Mcp-Param-Region"]; present {
				t.Fatalf("the call mirrored a definition that had run out: %v", s.headersAt(3))
			}
		})
	}
}

// TestAnOutcomeOfTheCallStandsWhenNoDefinitionIsToBeHad is the other half: a
// call that did have an outcome keeps it. A header the definition in hand could
// not render, and the server's own refusal of the headers it was sent, are both
// answers about the call itself, so a read that has no newer definition to give
// leaves them standing and sends nothing more.
func TestAnOutcomeOfTheCallStandsWhenNoDefinitionIsToBeHad(t *testing.T) {
	mismatch := errorFrame(CodeHeaderMismatch, "Header mismatch: The [Mcp-Param-Region] header is required.", nil)

	tests := []struct {
		name string
		// held is the definition the first listing states.
		held      map[string]any
		arguments map[string]any
		// frames follow the first listing.
		frames      []string
		wantErr     string
		wantMethods []string
	}{
		{
			name:        "a header that cannot be rendered, and the tool is gone",
			held:        regionTool("string"),
			arguments:   map[string]any{"region": 42},
			frames:      []string{toolsFrame()},
			wantErr:     "cannot be mirrored into the [Mcp-Param-Region] header as a [string]",
			wantMethods: []string{"server/discover", "tools/list", "tools/list"},
		},
		{
			name:        "a header that cannot be rendered, and the listing is refused",
			held:        regionTool("string"),
			arguments:   map[string]any{"region": 42},
			frames:      []string{methodNotFoundFrame()},
			wantErr:     "cannot be mirrored into the [Mcp-Param-Region] header as a [string]",
			wantMethods: []string{"server/discover", "tools/list", "tools/list"},
		},
		{
			name:        "headers the server refused, and the tool is gone",
			held:        plainTool("execute_sql"),
			arguments:   region(),
			frames:      []string{mismatch, toolsFrame()},
			wantErr:     "Header mismatch",
			wantMethods: []string{"server/discover", "tools/list", "tools/call", "tools/list"},
		},
		{
			name:        "headers the server refused, and the listing is refused",
			held:        plainTool("execute_sql"),
			arguments:   region(),
			frames:      []string{mismatch, methodNotFoundFrame()},
			wantErr:     "Header mismatch",
			wantMethods: []string{"server/discover", "tools/list", "tools/call", "tools/list"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t, append([]string{toolsFrame(tc.held)}, tc.frames...)...)
			newSteppedClock().attach(c)

			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			_, err = tools[0].Call(context.Background(), tc.arguments)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want the outcome of the call itself: %q", err, tc.wantErr)
			}
			if got := s.methods(); !slices.Equal(got, tc.wantMethods) {
				t.Fatalf("methods = %v, want %v", got, tc.wantMethods)
			}
		})
	}
}

// TestAToolOfAReplacedConnectionIsStillCalledWhenNoneIsToBeHad is the same
// for a definition the handshake outdated rather than the clock.
func TestAToolOfAReplacedConnectionIsStillCalledWhenNoneIsToBeHad(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(annotatedTool("Region")),
		discoverFrame(LatestProtocolVersion),
		toolsFrame(),
		errorFrame(jsonrpc.CodeInvalidParams, "Unknown tool: execute_sql", nil),
	)
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	c.Disconnect()

	_, err = tools[0].Call(context.Background(), region())
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, "Unknown tool: execute_sql") {
		t.Fatalf("error = %v, want the server's own answer", err)
	}
	want := []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestAnIntermediaryNeverSeesACallMirroredFromAStaleDefinition asserts the
// reason a definition is read again before its headers are rendered rather than
// after the call has failed. The endpoint routes on the mirrored header the way
// an intermediary in front of a server does, and turns a call away on HTTP's
// terms when it arrives without it: a plain 400 carrying no protocol answer,
// which says nothing a client could recover from. The server begins to mirror
// the parameter while the client holds the definition that did not, so the only
// thing standing between the call and that rejection is the lifetime the server
// gave the definition.
func TestAnIntermediaryNeverSeesACallMirroredFromAStaleDefinition(t *testing.T) {
	plain := map[string]any{"name": "execute_sql", "inputSchema": map[string]any{"type": "object"}}
	annotated := map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string", "x-mcp-header": "Region"},
			},
		},
	}

	var mu sync.Mutex
	mirroring := false
	rejected := 0
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		mu.Lock()
		required := mirroring
		mu.Unlock()
		switch request.method {
		case "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{},
				"ttlMs":             3600000,
				"cacheScope":        "private",
			}}))
		case "tools/list":
			tool := plain
			if required {
				tool = annotated
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"tools":      []any{tool},
				"ttlMs":      60000,
				"cacheScope": "private",
			}}))
		default:
			if required && request.headers.Get("Mcp-Param-Region") == "" {
				mu.Lock()
				rejected++
				mu.Unlock()
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "no route for a request without a region")
				return
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"content":    []any{map[string]any{"type": "text", "text": "done"}},
				"isError":    false,
			}}))
		}
	})

	client := Web(endpoint.URL)
	clock := newSteppedClock()
	clock.attach(client.Client)

	if _, err := client.CallTool(context.Background(), "execute_sql", region()); err != nil {
		t.Fatalf("first call: %v", err)
	}

	mu.Lock()
	mirroring = true
	mu.Unlock()
	clock.advance(60 * time.Second)

	result, err := client.CallTool(context.Background(), "execute_sql", region())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}

	mu.Lock()
	turnedAway := rejected
	mu.Unlock()
	if turnedAway != 0 {
		t.Fatalf("the intermediary turned %d call(s) away for the header a stale definition left out", turnedAway)
	}
	want := []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
	if got := endpoint.methods(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	calls := endpoint.requestsFor("tools/call")
	if got := calls[0].headers.Get("Mcp-Param-Region"); got != "" {
		t.Fatalf("the first call carried Mcp-Param-Region = %q, want none", got)
	}
	if got := calls[1].headers.Get("Mcp-Param-Region"); got != "us-west1" {
		t.Fatalf("the second call carried Mcp-Param-Region = %q, want us-west1", got)
	}
}

// notificationFrame builds a notification the server sends of its own accord.
func notificationFrame(method string) string {
	out, err := json.Marshal(map[string]any{"jsonrpc": jsonrpc.Version, "method": method})
	if err != nil {
		panic(err)
	}
	return string(out)
}

// TestAChangeTheServerAnnouncesOutdatesWhatItStated asserts the other way the
// specification has a listing go stale: a notification that the list of tools
// has changed outdates it at once, however much of its lifetime is left. The
// notification arrives on the stream of another exchange, which is where a
// client that holds no stream of its own hears it, and the definition the
// server changed is read again before the next call renders its headers.
func TestAChangeTheServerAnnouncesOutdatesWhatItStated(t *testing.T) {
	tests := []struct {
		name         string
		notification string
		reread       bool
	}{
		{name: "the list of tools has changed", notification: "notifications/tools/list_changed", reread: true},
		{name: "the list of resources has changed", notification: "notifications/resources/list_changed"},
		{name: "the list of prompts has changed", notification: "notifications/prompts/list_changed"},
		{name: "a message was logged", notification: "notifications/message"},
	}

	for _, tc := range tests {
		for _, by := range []string{"a call by name", "a call on a tool value"} {
			t.Run(tc.name+"/"+by, func(t *testing.T) {
				frames := []string{
					toolsFrame(plainTool("execute_sql")),
					notificationFrame(tc.notification),
					toolCallFrame("first"),
				}
				if tc.reread {
					frames = append(frames, toolsFrame(annotatedTool("Region")))
				}
				frames = append(frames, toolCallFrame("second"))

				c, s := discoveryClient(t, frames...)
				newSteppedClock().attach(c)

				tools, err := c.Tools(context.Background())
				if err != nil {
					t.Fatalf("tools: %v", err)
				}
				call := func() (*ToolResult, error) {
					if by == "a call by name" {
						return c.CallTool(context.Background(), "execute_sql", region())
					}
					return tools[0].Call(context.Background(), region())
				}
				for _, want := range []string{"first", "second"} {
					result, err := call()
					if err != nil {
						t.Fatalf("call: %v", err)
					}
					if result.Text() != want {
						t.Fatalf("text = %q, want %q", result.Text(), want)
					}
				}

				want := []string{"server/discover", "tools/list", "tools/call", "tools/call"}
				if tc.reread {
					want = []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
				}
				if got := s.methods(); !slices.Equal(got, want) {
					t.Fatalf("methods = %v, want %v", got, want)
				}
				got, present := s.headersAt(len(want) - 1)["Mcp-Param-Region"]
				if tc.reread && got != "us-west1" {
					t.Fatalf("Mcp-Param-Region = %q, want us-west1: the call mirrored a catalogue the server had changed", got)
				} else if !tc.reread && present {
					t.Fatalf("Mcp-Param-Region = %q, want none: nothing outdated the definition in hand", got)
				}
			})
		}
	}
}

// TestAChangeAnnouncedWhileTheCatalogueIsReadSettlesNothing asserts a listing
// the server outdates while it is being read is not kept. Nothing says which of
// its pages the server had already put together when it announced the change,
// so the call that follows reads the catalogue again; and the definition that
// call reads for itself is used whatever the server announces on its way, so a
// server that announces a change with every listing is still called.
func TestAChangeAnnouncedWhileTheCatalogueIsReadSettlesNothing(t *testing.T) {
	c, s := discoveryClient(t,
		pagedToolsFrame(scriptedLifetimeMs, "page-2", plainTool("execute_sql")),
		notificationFrame(toolsChangedNotification),
		pagedToolsFrame(scriptedLifetimeMs, "", plainTool("summarize")),
		notificationFrame(toolsChangedNotification),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	newSteppedClock().attach(c)

	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}
	result, err := c.CallTool(context.Background(), "execute_sql", region())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "tools/list", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if got := s.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestWhatTheServerAdvertisesIsKeptForTheLifetimeItGave asserts the discover
// result is the cacheable result the specification makes it. What it advertises
// answers a caller for as long as the server said it may be kept, and is asked
// for again, over the channel already open, the next time a caller wants it
// after that. The server advertises something else the second time, so what the
// caller is told says which answer it was given.
func TestWhatTheServerAdvertisesIsKeptForTheLifetimeItGave(t *testing.T) {
	tests := []struct {
		name     string
		lifetime any
		elapsed  time.Duration
		reread   bool
	}{
		{name: "a millisecond short of the lifetime is fresh", lifetime: 60000, elapsed: 59999 * time.Millisecond},
		{name: "the lifetime itself is stale", lifetime: 60000, elapsed: 60000 * time.Millisecond, reread: true},
		{name: "no lifetime stated is none", lifetime: nil, elapsed: 0, reread: true},
		{name: "a lifetime of zero is stale on arrival", lifetime: 0, elapsed: 0, reread: true},
		{name: "a negative lifetime is read as zero", lifetime: -1, elapsed: 0, reread: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(
				discoverFrameStating("As first advertised.", tc.lifetime, LatestProtocolVersion),
				discoverFrameStating("As advertised since.", scriptedLifetimeMs, LatestProtocolVersion),
			)
			c := newScriptedClient(s)
			clock := newSteppedClock()
			clock.attach(c)

			// The call that settles the connection is answered with what it
			// settled, however short a lifetime that was given.
			first, err := c.Instructions(context.Background())
			if err != nil {
				t.Fatalf("instructions: %v", err)
			}
			if first != "As first advertised." {
				t.Fatalf("instructions = %q, want what the handshake settled", first)
			}
			clock.advance(tc.elapsed)

			second, err := c.Instructions(context.Background())
			if err != nil {
				t.Fatalf("instructions: %v", err)
			}
			wantText, wantMethods := "As first advertised.", []string{"server/discover"}
			if tc.reread {
				wantText, wantMethods = "As advertised since.", []string{"server/discover", "server/discover"}
			}
			if second != wantText {
				t.Fatalf("instructions = %q, want %q", second, wantText)
			}
			if got := s.methods(); !slices.Equal(got, wantMethods) {
				t.Fatalf("methods = %v, want %v", got, wantMethods)
			}
			// It was asked for again over the channel already open, not over a
			// new one: a transport such as stdio would have had its server
			// stopped and started to be asked a question.
			if connects, disconnects, connected := s.lifecycle(); connects != 1 || disconnects != 0 || !connected {
				t.Fatalf("connects = %d, disconnects = %d, connected = %v: want the one channel, still open",
					connects, disconnects, connected)
			}
			if !c.Connected() {
				t.Fatal("asking again must leave a connection standing")
			}
		})
	}
}

// TestEveryAdvertisedDetailIsReadAgainOnceItHasRunOut asserts the lifetime
// governs every answer read out of the discover result, not one of them.
func TestEveryAdvertisedDetailIsReadAgainOnceItHasRunOut(t *testing.T) {
	askers := map[string]func(c *Client) error{
		"capabilities": func(c *Client) error {
			_, err := c.Capabilities(context.Background())
			return err
		},
		"server info": func(c *Client) error {
			_, err := c.ServerInfo(context.Background())
			return err
		},
		"instructions": func(c *Client) error {
			_, err := c.Instructions(context.Background())
			return err
		},
	}

	for name, ask := range askers {
		t.Run(name, func(t *testing.T) {
			s := newScriptedTransport(
				discoverFrameStating("Be nice.", 1000, LatestProtocolVersion),
				discoverFrameStating("Be nice.", 1000, LatestProtocolVersion),
			)
			c := newScriptedClient(s)
			clock := newSteppedClock()
			clock.attach(c)

			if err := c.Connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}
			if err := ask(c); err != nil {
				t.Fatalf("fresh: %v", err)
			}
			if got := s.methods(); !slices.Equal(got, []string{"server/discover"}) {
				t.Fatalf("methods = %v: a fresh answer was asked for again", got)
			}
			clock.advance(time.Second)
			if err := ask(c); err != nil {
				t.Fatalf("stale: %v", err)
			}
			if got := s.methods(); !slices.Equal(got, []string{"server/discover", "server/discover"}) {
				t.Fatalf("methods = %v: a stale answer was given without asking again", got)
			}
		})
	}
}

// TestWhatAnInitializeResultAdvertisesHasNoLifetime asserts the lifetime belongs
// to the revision that defines it. An initialize result states none and is not
// one of the results the specification makes cacheable: what it advertises was
// agreed for the session the handshake opened.
func TestWhatAnInitializeResultAdvertisesHasNoLifetime(t *testing.T) {
	s := newScriptedTransport(methodNotFoundFrame(), initializeFrame(ProtocolV20251125))
	c := newScriptedClient(s)
	clock := newSteppedClock()
	clock.attach(c)

	for range 2 {
		instructions, err := c.Instructions(context.Background())
		if err != nil || instructions != "Be nice." {
			t.Fatalf("instructions = %q err=%v", instructions, err)
		}
		clock.advance(24 * time.Hour)
	}
	want := []string{"server/discover", "initialize", "notifications/initialized"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestAStaleAdvertisementThatCannotBeReadAgainIsReported asserts a failure to
// ask again is the caller's to know about: the answer in hand has run out, and
// handing it over regardless would pass off what the server said once as what
// it says now.
func TestAStaleAdvertisementThatCannotBeReadAgainIsReported(t *testing.T) {
	s := newScriptedTransport(discoverFrameStating("Be nice.", 1000, LatestProtocolVersion))
	c := newScriptedClient(s)
	clock := newSteppedClock()
	clock.attach(c)

	if _, err := c.Instructions(context.Background()); err != nil {
		t.Fatalf("instructions: %v", err)
	}
	clock.advance(time.Second)
	s.failOn["server/discover"] = NewTransportError("the channel is gone", nil)

	instructions, err := c.Instructions(context.Background())
	if err == nil {
		t.Fatalf("instructions = %q, want the failure to ask again", instructions)
	}
	if c.Connected() {
		t.Fatal("a handshake that failed must not leave a connection standing")
	}
}

// TestAskingWhatTheServerAdvertisesSendsNothingForACallerThatHasGone asserts
// the accessors that may ask the server again give up the way every request
// does: a caller whose context is already done is told so, and nothing is sent
// on its behalf.
func TestAskingWhatTheServerAdvertisesSendsNothingForACallerThatHasGone(t *testing.T) {
	askers := map[string]func(c *Client, ctx context.Context) error{
		"capabilities": func(c *Client, ctx context.Context) error {
			_, err := c.Capabilities(ctx)
			return err
		},
		"server info": func(c *Client, ctx context.Context) error {
			_, err := c.ServerInfo(ctx)
			return err
		},
		"instructions": func(c *Client, ctx context.Context) error {
			_, err := c.Instructions(ctx)
			return err
		},
		"a listing": func(c *Client, ctx context.Context) error {
			_, err := c.Tools(ctx)
			return err
		},
	}

	for name, ask := range askers {
		t.Run(name, func(t *testing.T) {
			s := newScriptedTransport(discoverFrame(LatestProtocolVersion), toolsFrame())
			c := newScriptedClient(s)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := ask(c, ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want the caller's own cancellation", err)
			}
			if got := s.methods(); len(got) != 0 {
				t.Fatalf("methods = %v, want nothing sent for a caller that has gone", got)
			}
		})
	}
}

// TestAskingWhatTheServerAdvertisesReportsAHandshakeThatFailed asserts a
// connection that cannot be settled is the answer, rather than an empty one.
func TestAskingWhatTheServerAdvertisesReportsAHandshakeThatFailed(t *testing.T) {
	s := newScriptedTransport(`{"jsonrpc":"1.0","id":1,"result":{}}`)
	c := newScriptedClient(s)

	instructions, err := c.Instructions(context.Background())
	if err == nil {
		t.Fatalf("instructions = %q, want the failure of the handshake", instructions)
	}
	if c.Connected() {
		t.Fatal("a handshake that failed must not leave a connection standing")
	}
}

// TestASessionThatCannotBeRenegotiatedFailsTheRequest asserts the request a
// forgotten session interrupts reports the handshake that failed under it,
// rather than being repeated over a connection that does not stand.
func TestASessionThatCannotBeRenegotiatedFailsTheRequest(t *testing.T) {
	c, s := discoveryClient(t, `{"jsonrpc":"1.0","id":1,"result":{}}`)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	s.failOnce["tools/list"] = errSessionExpired

	if _, err := c.Tools(context.Background()); err == nil {
		t.Fatal("expected the listing to fail with the handshake that failed under it")
	}
	want := []string{"server/discover", "server/discover"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: the request was repeated over no connection", got, want)
	}
	if c.Connected() {
		t.Fatal("a handshake that failed must not leave a connection standing")
	}
}

// TestAMalformedListingIsRefused asserts a page that is not what the
// specification describes fails the listing with a message that says so, and
// records nothing: a catalogue is read from an untrusted server, and one that
// cannot be read is not a catalogue that states no tools.
func TestAMalformedListingIsRefused(t *testing.T) {
	page := func(result string) string {
		return `{"jsonrpc":"2.0","id":` + scriptRequestID + `,"result":` + result + `}`
	}
	tests := []struct {
		name    string
		frame   string
		wantErr string
	}{
		{name: "the result is not an object", frame: page(`["execute_sql"]`), wantErr: "invalid tools/list response from server"},
		{name: "the page is not an array", frame: page(`{"tools":{"name":"execute_sql"}}`), wantErr: "invalid tools/list response from server"},
		{name: "the page is absent", frame: page(`{"ttlMs":60000}`), wantErr: "invalid tools/list response from server"},
		{name: "an entry is not an object", frame: page(`{"tools":["execute_sql"]}`), wantErr: "invalid tools payload from server"},
		{name: "an entry has no name", frame: page(`{"tools":[{"title":"Runs SQL"}]}`), wantErr: "invalid tool payload from server"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t, tc.frame, toolsFrame(annotatedTool("Region")), toolCallFrame("done"))

			tools, err := c.Tools(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("tools = %+v, error = %v, want %q", tools, err, tc.wantErr)
			}
			// Nothing was recorded of it, so the call that follows reads the
			// catalogue for itself.
			if _, err := c.CallTool(context.Background(), "execute_sql", region()); err != nil {
				t.Fatalf("call: %v", err)
			}
			want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
			if got := s.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v", got, want)
			}
			if got := s.headersAt(3)["Mcp-Param-Region"]; got != "us-west1" {
				t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
			}
		})
	}
}

// FuzzLifetimeOf drives the reader of the ttlMs hint over whatever a server
// could put in the member. It reads a result an untrusted server supplied, so it
// must never panic and never state a negative lifetime, and what it states is
// held to the specification in exact arithmetic rather than to its own: a whole
// number of milliseconds above zero is that lifetime (or the longest one a
// duration holds, never one that wrapped round), and everything else is none.
func FuzzLifetimeOf(f *testing.F) {
	seeds := []string{
		`60000`, `0`, `-1`, `1`, `6e4`, `60000.0`, `1.5`, `1e-3`, `-0`, `0e99999`,
		`9223372036854775807`, `9223372036854775808`, `9223372036855`, `9223372036854`, `1e30`, `1e400`,
		`"60000"`, `null`, `true`, `[]`, `{}`, `{"ttlMs":5}`, ``, ` 7 `, `+5`, `0x10`, `1_000`, `NaN`, `Infinity`,
		`60000,"ttlMs":0`, `0,"ttlMs":60000`,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	longest := new(big.Rat).SetInt64(math.MaxInt64 / int64(time.Millisecond))

	f.Fuzz(func(t *testing.T, member string) {
		document := `{"tools":[],"ttlMs":` + member + `}`
		got := lifetimeOf(json.RawMessage(document))
		if got < 0 {
			t.Fatalf("lifetimeOf(%s) = %d, want no negative lifetime", document, got)
		}

		// The member as a decoder reads it: the last one wins when it is stated
		// more than once, and a document that is not JSON states nothing.
		var decoded struct {
			TTL json.RawMessage `json:"ttlMs"`
		}
		if err := json.Unmarshal([]byte(document), &decoded); err != nil {
			if got != 0 {
				t.Fatalf("lifetimeOf(%s) = %d for a document that is not JSON, want 0", document, got)
			}
			return
		}
		// A JSON number opens with a minus sign or a digit, and nothing else does.
		text := strings.TrimSpace(string(decoded.TTL))
		if text == "" || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) {
			if got != 0 {
				t.Fatalf("lifetimeOf(%s) = %d for a member that is not a number, want 0", document, got)
			}
			return
		}

		value, ok := exactNumber(text)
		if !ok {
			return
		}
		want := time.Duration(0)
		switch {
		case !value.IsInt() || value.Sign() <= 0:
		case value.Cmp(new(big.Rat).SetInt64(math.MaxInt64)) > 0:
			// A whole number an integer cannot hold is a hint this client does
			// not date a result with.
		case value.Cmp(longest) > 0:
			want = math.MaxInt64
		default:
			want = time.Duration(value.Num().Int64()) * time.Millisecond
		}
		if got != want {
			t.Fatalf("lifetimeOf(%s) = %d, want %d", document, got, want)
		}
	})
}
