package client

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// This file covers the authorization context of what the client keeps. The
// specification lets a result scoped to one context be reused in that context
// and forbids it in any other, and a different access token is a different
// context. Every endpoint below answers by the bearer token it is shown, the
// way a server whose catalogue depends on who is asking does, so what a caller
// is told, and what goes out on the wire under its token, says whose answer the
// client was working from.

// identityOf returns the token a request presented, without the scheme.
func identityOf(request recordedRequest) string {
	return strings.TrimPrefix(request.headers.Get("Authorization"), "Bearer ")
}

// regionMirroringTool is a definition that asks for its region argument to be
// mirrored into a header, and unmirroredTool the same tool asking for nothing.
func regionMirroringTool() map[string]any {
	return map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string", "x-mcp-header": "Region"},
			},
		},
	}
}

func unmirroredTool() map[string]any {
	return map[string]any{"name": "execute_sql", "inputSchema": map[string]any{"type": "object"}}
}

// unusableTool is a definition this client refuses: its annotation sits under an
// array's items, where no property chain reaches it.
func unusableTool(name string) map[string]any {
	return map[string]any{
		"name": name,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"rows": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":       "object",
						"properties": map[string]any{"region": map[string]any{"type": "string", "x-mcp-header": "Region"}},
					},
				},
			},
		},
	}
}

// newIdentityEndpoint starts a discovery-era endpoint whose every answer depends
// on the token it is shown. The caller "alice" is advertised a tool that mirrors
// nothing, and one this client refuses; every other caller is advertised the
// same tool mirroring its region, and is turned away on HTTP's terms, as an
// intermediary routing on the header would, when a call arrives without it.
// Everything is stated to be kept for an hour under private scope, so nothing
// below is read again because it ran out.
func newIdentityEndpoint(t *testing.T) *recordingEndpoint {
	t.Helper()
	return newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		who := identityOf(request)
		switch request.method {
		case "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{"tools": map[string]any{"owner": who}},
				"instructions":      "Instructions for " + who + ".",
				"ttlMs":             3600000,
				"cacheScope":        "private",
				"_meta": map[string]any{
					MetaServerInfo: map[string]any{"name": "Server of " + who, "version": "1.0.0"},
				},
			}}))
		case "tools/list":
			tools := []any{regionMirroringTool()}
			if who == "alice" {
				tools = []any{unmirroredTool(), unusableTool("only_for_alice")}
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"tools":      tools,
				"ttlMs":      3600000,
				"cacheScope": "private",
			}}))
		default:
			if who != "alice" && request.headers.Get("Mcp-Param-Region") == "" {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "no route for a request without a region")
				return
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"content":    []any{map[string]any{"type": "text", "text": "done for " + who}},
				"isError":    false,
			}}))
		}
	})
}

// wire returns every recorded request as "identity method", in order.
func (e *recordingEndpoint) wire() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.requests))
	for _, request := range e.requests {
		method := request.method
		if method == "" {
			method = request.httpMethod
		}
		out = append(out, identityOf(request)+" "+method)
	}
	return out
}

// identitySwitch is one way a caller changes the token a client presents.
type identitySwitch struct {
	name string
	// on returns what makes the given client present a token from now on.
	on func(w *WebClient) func(token string)
}

// identitySwitches are the two ways there are: a token set outright, and a
// callback that answers with another one. The callback is installed once and
// reads the identity of the moment, which is how a caller that acts for several
// users, or whose token is rotated underneath it, changes context without ever
// telling the client.
func identitySwitches() []identitySwitch {
	return []identitySwitch{
		{
			name: "a token set outright",
			on: func(w *WebClient) func(string) {
				return func(token string) { w.WithToken(token) }
			},
		},
		{
			name: "a callback resolving another token",
			on: func(w *WebClient) func(string) {
				var current atomic.Value
				current.Store("")
				w.WithTokenFunc(func() string { return current.Load().(string) })
				return func(token string) { current.Store(token) }
			},
		},
	}
}

// TestWhatOneTokenWasToldIsNotToldToAnother asserts what the server advertised
// to one caller never answers another. Every accessor that reads the result a
// connection was settled with is asked under each identity in turn: the ones
// that can ask the server again do so under the token now presented, and the
// ones that send nothing return nothing rather than the other caller's record.
func TestWhatOneTokenWasToldIsNotToldToAnother(t *testing.T) {
	for _, how := range identitySwitches() {
		t.Run(how.name, func(t *testing.T) {
			endpoint := newIdentityEndpoint(t)
			client := Web(endpoint.URL)
			become := how.on(client)
			ctx := context.Background()

			advertisedTo := func(who string) {
				t.Helper()
				instructions, err := client.Instructions(ctx)
				if err != nil {
					t.Fatalf("instructions: %v", err)
				}
				if want := "Instructions for " + who + "."; instructions != want {
					t.Fatalf("instructions = %q, want %q", instructions, want)
				}
				capabilities, err := client.Capabilities(ctx)
				if err != nil {
					t.Fatalf("capabilities: %v", err)
				}
				if owner := capabilities["tools"].(map[string]any)["owner"]; owner != who {
					t.Fatalf("capabilities were advertised to %v, want %s", owner, who)
				}
				info, err := client.ServerInfo(ctx)
				if err != nil {
					t.Fatalf("server info: %v", err)
				}
				if want := "Server of " + who; info.Name != want {
					t.Fatalf("server info = %q, want %q", info.Name, want)
				}
				record := client.DiscoverResult()
				if record == nil || record.Instructions != "Instructions for "+who+"." {
					t.Fatalf("discover result = %+v, want the one settled for %s", record, who)
				}
			}
			nothingToShow := func(after string) {
				t.Helper()
				if record := client.DiscoverResult(); record != nil {
					t.Fatalf("discover result after becoming %s = %+v, want none: it was told to someone else", after, record)
				}
			}

			become("alice")
			advertisedTo("alice")

			become("bob")
			nothingToShow("bob")
			advertisedTo("bob")

			// Going back is no different from going anywhere else: the cache
			// of the token in between is not the first token's either.
			become("alice")
			nothingToShow("alice")
			advertisedTo("alice")

			want := []string{"alice server/discover", "bob server/discover", "alice server/discover"}
			if got := endpoint.wire(); !slices.Equal(got, want) {
				t.Fatalf("wire = %v, want %v", got, want)
			}
		})
	}
}

// TestACallIsMirroredFromTheDefinitionItsOwnTokenWasGiven asserts a definition
// read for one caller settles the headers of no other caller's call. The two
// are advertised different definitions of the same tool, and the endpoint turns
// away on HTTP's terms a call that arrives without the header its caller's
// definition asks for, so a call mirrored from the wrong one fails outright and
// shows on the wire.
func TestACallIsMirroredFromTheDefinitionItsOwnTokenWasGiven(t *testing.T) {
	for _, how := range identitySwitches() {
		for _, by := range []string{"a call by name", "a call on a tool value"} {
			t.Run(how.name+"/"+by, func(t *testing.T) {
				endpoint := newIdentityEndpoint(t)
				client := Web(endpoint.URL)
				become := how.on(client)
				ctx := context.Background()

				become("alice")
				tools, err := client.Tools(ctx)
				if err != nil {
					t.Fatalf("tools: %v", err)
				}
				if len(tools) != 1 || tools[0].Name != "execute_sql" {
					t.Fatalf("tools = %+v, want the one tool alice may use", tools)
				}
				if excluded := client.ExcludedTools(); len(excluded) != 1 || excluded[0].Name != "only_for_alice" {
					t.Fatalf("excluded = %+v, want only_for_alice", excluded)
				}
				call := func() (*ToolResult, error) {
					if by == "a call by name" {
						return client.CallTool(ctx, "execute_sql", region())
					}
					return tools[0].Call(ctx, region())
				}
				if result, err := call(); err != nil || result.Text() != "done for alice" {
					t.Fatalf("alice's call = %v, %v", result, err)
				}

				become("bob")
				// The name of a tool only alice was advertised is not bob's to
				// be shown, whether or not bob has listed anything yet.
				if excluded := client.ExcludedTools(); len(excluded) != 0 {
					t.Fatalf("excluded after becoming bob = %+v, want none", excluded)
				}
				result, err := call()
				if err != nil {
					t.Fatalf("bob's call: %v", err)
				}
				if result.Text() != "done for bob" {
					t.Fatalf("text = %q, want done for bob", result.Text())
				}
				if excluded := client.ExcludedTools(); len(excluded) != 0 {
					t.Fatalf("excluded for bob = %+v, want none", excluded)
				}

				want := []string{
					"alice server/discover", "alice tools/list", "alice tools/call",
					"bob server/discover", "bob tools/list", "bob tools/call",
				}
				if got := endpoint.wire(); !slices.Equal(got, want) {
					t.Fatalf("wire = %v, want %v", got, want)
				}
				calls := endpoint.requestsFor("tools/call")
				if got := calls[0].headers.Get("Mcp-Param-Region"); got != "" {
					t.Fatalf("alice's call carried Mcp-Param-Region = %q, want none", got)
				}
				if got := calls[1].headers.Get("Mcp-Param-Region"); got != "us-west1" {
					t.Fatalf("bob's call carried Mcp-Param-Region = %q, want us-west1", got)
				}
			})
		}
	}
}

// TestTheSameTokenIsTheSameContext asserts the context is the token and not the
// act of setting it: a caller that sets again the token it already presents has
// changed nothing, and what it was told is still its own.
func TestTheSameTokenIsTheSameContext(t *testing.T) {
	endpoint := newIdentityEndpoint(t)
	client := Web(endpoint.URL).WithToken("alice")
	ctx := context.Background()

	if _, err := client.CallTool(ctx, "execute_sql", region()); err != nil {
		t.Fatalf("call: %v", err)
	}
	client.WithToken("alice")
	if record := client.DiscoverResult(); record == nil || record.Instructions != "Instructions for alice." {
		t.Fatalf("discover result = %+v, want the one alice settled", record)
	}
	if instructions, err := client.Instructions(ctx); err != nil || instructions != "Instructions for alice." {
		t.Fatalf("instructions = %q err=%v", instructions, err)
	}
	if _, err := client.CallTool(ctx, "execute_sql", region()); err != nil {
		t.Fatalf("call: %v", err)
	}

	want := []string{"alice server/discover", "alice tools/list", "alice tools/call", "alice tools/call"}
	if got := endpoint.wire(); !slices.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}

// newSessionEndpoint starts an initialize-era endpoint that opens a session for
// whoever initializes and names it after them.
func newSessionEndpoint(t *testing.T) *recordingEndpoint {
	t.Helper()
	return newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		who := identityOf(request)
		switch {
		case request.httpMethod == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case request.method == "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"error": map[string]any{
				"code": -32601, "message": "Method not found.",
			}}))
		case request.method == "initialize":
			w.Header().Set(sessionHeader, "session-of-"+who)
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"protocolVersion": ProtocolV20251125,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "Server of " + who, "version": "1.0.0"},
				"instructions":    "Instructions for " + who + ".",
			}}))
		case len(request.id) == 0:
			w.WriteHeader(http.StatusAccepted)
		default:
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"tools": []any{},
			}}))
		}
	})
}

// TestASessionIsNeverPresentedUnderAnotherToken asserts the session a handshake
// opened goes the way of everything else one caller was given. It was opened
// for the token that initialized, so it is released with that token and the
// next caller initializes a session of its own: no request on the wire shows a
// server one caller's session under another caller's token.
func TestASessionIsNeverPresentedUnderAnotherToken(t *testing.T) {
	for _, how := range identitySwitches() {
		t.Run(how.name, func(t *testing.T) {
			endpoint := newSessionEndpoint(t)
			client := Web(endpoint.URL)
			become := how.on(client)
			ctx := context.Background()

			become("alice")
			if _, err := client.Tools(ctx); err != nil {
				t.Fatalf("alice's tools: %v", err)
			}
			if record := client.InitializeResult(); record == nil || record.Instructions != "Instructions for alice." {
				t.Fatalf("initialize result = %+v, want the one alice settled", record)
			}

			become("bob")
			if record := client.InitializeResult(); record != nil {
				t.Fatalf("initialize result after becoming bob = %+v, want none", record)
			}
			if _, err := client.Tools(ctx); err != nil {
				t.Fatalf("bob's tools: %v", err)
			}
			if instructions, err := client.Instructions(ctx); err != nil || instructions != "Instructions for bob." {
				t.Fatalf("instructions = %q err=%v", instructions, err)
			}

			want := []string{
				"alice server/discover", "alice initialize", "alice notifications/initialized", "alice tools/list",
				"alice DELETE", "bob initialize", "bob notifications/initialized", "bob tools/list",
			}
			if got := endpoint.wire(); !slices.Equal(got, want) {
				t.Fatalf("wire = %v, want %v", got, want)
			}
			endpoint.mu.Lock()
			defer endpoint.mu.Unlock()
			for index, request := range endpoint.requests {
				session := request.headers.Get(sessionHeader)
				if session != "" && session != "session-of-"+identityOf(request) {
					t.Fatalf("request %d showed %s under the token of %s", index, session, identityOf(request))
				}
			}
			if got := endpoint.requests[4].headers.Get(sessionHeader); got != "session-of-alice" {
				t.Fatalf("the session released was %q, want session-of-alice", got)
			}
			if got := endpoint.requests[5].headers.Get(sessionHeader); got != "" {
				t.Fatalf("bob initialized under the session %q, want none", got)
			}
		})
	}
}

// TestEveryFrameOfAnExchangePresentsOneToken asserts the token is resolved once
// for an exchange rather than once for each frame. A handshake is several
// frames, and what it settles is recorded under one authorization context: a
// callback that answers differently every time it is asked would otherwise
// spread one connection over several tokens, and no record of whose it is would
// be true.
func TestEveryFrameOfAnExchangePresentsOneToken(t *testing.T) {
	endpoint := newSessionEndpoint(t)
	var asked atomic.Int64
	client := Web(endpoint.URL).WithTokenFunc(func() string {
		return "token-" + strconv.FormatInt(asked.Add(1), 10)
	})

	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	want := []string{"token-1 server/discover", "token-1 initialize", "token-1 notifications/initialized"}
	if got := endpoint.wire(); !slices.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}

// TestAListingIsReadUnderOneToken asserts a cursor goes nowhere but back to the
// caller it was handed to. The token changes while a listing is between two
// pages: the second page is not asked for with the first caller's cursor under
// the second caller's token, and the pages of the two are not returned as one
// catalogue. The listing is read again from the start for the caller now
// presented.
func TestAListingIsReadUnderOneToken(t *testing.T) {
	tests := []struct {
		name string
		// kind is the plural primitive name: the list method and the result key.
		kind string
		// list reads the whole listing and returns the names it carries.
		list func(client *WebClient) ([]string, error)
	}{
		{
			name: "tools",
			kind: "tools",
			list: func(client *WebClient) ([]string, error) {
				tools, err := client.Tools(context.Background())
				names := make([]string, 0, len(tools))
				for _, tool := range tools {
					names = append(names, tool.Name)
				}
				return names, err
			},
		},
		{
			name: "resources",
			kind: "resources",
			list: func(client *WebClient) ([]string, error) {
				resources, err := client.Resources(context.Background())
				names := make([]string, 0, len(resources))
				for _, resource := range resources {
					names = append(names, resource.Name)
				}
				return names, err
			},
		},
		{
			name: "prompts",
			kind: "prompts",
			list: func(client *WebClient) ([]string, error) {
				prompts, err := client.Prompts(context.Background())
				names := make([]string, 0, len(prompts))
				for _, prompt := range prompts {
					names = append(names, prompt.Name)
				}
				return names, err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var current atomic.Value
			current.Store("alice")
			entry := func(name string) map[string]any {
				return map[string]any{"name": name, "uri": "file:///" + name}
			}

			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				who := identityOf(request)
				switch request.method {
				case "server/discover":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"supportedVersions": []any{LatestProtocolVersion},
						"capabilities":      map[string]any{},
						"ttlMs":             3600000,
						"cacheScope":        "private",
					}}))
				case tc.kind + "/list":
					result := map[string]any{
						"resultType": "complete",
						tc.kind:      []any{entry("first_page_of_" + who)},
						"ttlMs":      3600000,
						"cacheScope": "private",
					}
					switch {
					case strings.Contains(request.body, "cursor-of-"):
						result[tc.kind] = []any{entry("second_page_of_" + who)}
					case who == "alice":
						// The caller becomes someone else once alice's first
						// page is on its way back, which is between the two.
						result["nextCursor"] = "cursor-of-alice"
						current.Store("bob")
					}
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": result}))
				}
			})

			client := Web(endpoint.URL).WithTokenFunc(func() string { return current.Load().(string) })
			names, err := tc.list(client)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if want := []string{"first_page_of_bob"}; !slices.Equal(names, want) {
				t.Fatalf("listed %v, want %v: the listing of one caller", names, want)
			}
			method := tc.kind + "/list"
			want := []string{"alice server/discover", "alice " + method, "bob server/discover", "bob " + method}
			if got := endpoint.wire(); !slices.Equal(got, want) {
				t.Fatalf("wire = %v, want %v", got, want)
			}
			for index, request := range endpoint.requestsFor(method) {
				if identityOf(request) == "bob" && strings.Contains(request.body, "cursor-of-alice") {
					t.Fatalf("%s %d showed alice's cursor under bob's token: %s", method, index, request.body)
				}
			}
		})
	}
}

// TestAListingWhoseTokenKeepsChangingIsReported asserts the listing is read
// again once and not for ever: a callback that never answers twice alike gives
// no caller a whole catalogue, which is reported rather than chased.
func TestAListingWhoseTokenKeepsChangingIsReported(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		switch request.method {
		case "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{},
			}}))
		case "tools/list":
			result := map[string]any{
				"resultType": "complete",
				"tools":      []any{map[string]any{"name": "a_tool"}},
			}
			// A first page hands out a cursor. A page asked for with one is the
			// last, so a client that does present a cursor is shown to have done
			// it rather than followed for ever.
			if !strings.Contains(request.body, "cursor-of-") {
				result["nextCursor"] = "cursor-of-" + identityOf(request)
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": result}))
		}
	})

	var asked atomic.Int64
	client := Web(endpoint.URL).WithTokenFunc(func() string {
		return "token-" + strconv.FormatInt(asked.Add(1), 10)
	})

	tools, err := client.Tools(context.Background())
	if err == nil {
		t.Fatalf("tools = %+v, want the listing to be reported as belonging to no one caller", tools)
	}
	if !strings.Contains(err.Error(), "kept changing") {
		t.Fatalf("error = %q", err.Error())
	}
	for index, request := range endpoint.requestsFor("tools/list") {
		if strings.Contains(request.body, "cursor-of-") {
			t.Fatalf("tools/list %d presented a cursor under another token: %s", index, request.body)
		}
	}
	want := []string{
		"token-1 server/discover", "token-1 tools/list", "token-2 server/discover",
		"token-3 server/discover", "token-3 tools/list", "token-4 server/discover",
	}
	if got := endpoint.wire(); !slices.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}

// TestAuthorizationContextsOfTheHTTPTransport asserts the contract of the
// transport hook the client builds all of this on: the context changes exactly
// when the token settled differs from the one settled before, asking which
// context the token of the moment belongs to settles nothing, and the frames in
// flight go on presenting what was settled.
func TestAuthorizationContextsOfTheHTTPTransport(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})
	tr := NewHTTPTransport(endpoint.URL)
	ctx := context.Background()

	steps := []struct {
		token       string
		wantContext int64
	}{
		{token: "alice", wantContext: 1},
		{token: "alice", wantContext: 1},
		{token: "bob", wantContext: 2},
		{token: "bob", wantContext: 2},
		{token: "alice", wantContext: 3},
		{token: "", wantContext: 4},
		{token: "", wantContext: 4},
		{token: "alice", wantContext: 5},
	}
	if got := tr.AuthorizationContext(); got != 1 {
		t.Fatalf("the context before any token was settled = %d, want the first one, 1", got)
	}
	for index, step := range steps {
		tr.WithToken(step.token)
		if got := tr.AuthorizationContext(); got != step.wantContext {
			t.Fatalf("step %d: the context %q would be settled in = %d, want %d", index, step.token, got, step.wantContext)
		}
		if got := tr.SettleAuthorization(); got != step.wantContext {
			t.Fatalf("step %d: %q was settled in context %d, want %d", index, step.token, got, step.wantContext)
		}
	}

	// A token set while an exchange is in flight is asked about, not settled:
	// the frames go on presenting the token the exchange was settled with.
	tr.WithToken("mallory")
	if got := tr.AuthorizationContext(); got != 6 {
		t.Fatalf("the context mallory would be settled in = %d, want 6", got)
	}
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := identityOf(endpoint.lastRequest(t, "tools/list")); got != "alice" {
		t.Fatalf("the frame in flight presented %q, want the token settled for it, alice", got)
	}
	if got := tr.SettleAuthorization(); got != 6 {
		t.Fatalf("mallory was settled in context %d, want 6", got)
	}
	if err := tr.Send(ctx, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := identityOf(endpoint.lastRequest(t, "tools/list")); got != "mallory" {
		t.Fatalf("the frame presented %q, want mallory", got)
	}
}

// TestATransportDrivenWithoutAClientResolvesItsTokenForEveryRequest asserts the
// transport on its own behaves as it always has: nothing settles a token for
// it, so every request presents what the callback answers at that moment.
func TestATransportDrivenWithoutAClientResolvesItsTokenForEveryRequest(t *testing.T) {
	endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
	})
	tr := NewHTTPTransport(endpoint.URL)
	var asked atomic.Int64
	tr.WithTokenFunc(func() string { return "token-" + strconv.FormatInt(asked.Add(1), 10) })

	for id := 1; id <= 2; id++ {
		frame := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/list"}`
		if err := tr.Send(context.Background(), frame); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	want := []string{"token-1 tools/list", "token-2 tools/list"}
	if got := endpoint.wire(); !slices.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
}

// credentialedTransport is a scripted transport that presents a credential of
// its own, the way a custom transport that signs its frames or dials with a
// client certificate does. It takes part through the same hook the HTTP
// transport does, and records the credential every frame went out under.
type credentialedTransport struct {
	*scriptedTransport

	mu         sync.Mutex
	credential string
	settled    string
	contexts   int64
	presented  []string
}

var _ AuthorizationAware = (*credentialedTransport)(nil)

func (c *credentialedTransport) become(credential string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credential = credential
}

func (c *credentialedTransport) SettleAuthorization() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.contexts == 0 || c.settled != c.credential {
		c.contexts++
	}
	c.settled = c.credential
	return c.contexts
}

func (c *credentialedTransport) AuthorizationContext() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.contexts != 0 && c.settled == c.credential {
		return c.contexts
	}
	return c.contexts + 1
}

func (c *credentialedTransport) SendWithHeaders(ctx context.Context, message string, headers map[string]string) error {
	c.mu.Lock()
	c.presented = append(c.presented, c.settled+" "+frameMethod(message))
	c.mu.Unlock()
	return c.scriptedTransport.SendWithHeaders(ctx, message, headers)
}

func (c *credentialedTransport) wire() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.presented...)
}

// TestAnyTransportThatPresentsACredentialTakesPart asserts the authorization
// context is the transport hook's to state and not a property of HTTP. A custom
// transport that implements the hook gets everything the HTTP transport does: a
// connection settled under one credential is given up when another is
// presented, the channel is torn down so the transport can let go of whatever
// it opened for the first, and nothing the first was told or stated answers the
// second.
func TestAnyTransportThatPresentsACredentialTakesPart(t *testing.T) {
	transport := &credentialedTransport{scriptedTransport: newScriptedTransport(
		discoverFrameStating("Instructions for alice.", scriptedLifetimeMs, LatestProtocolVersion),
		toolsFrame(plainTool("execute_sql")),
		discoverFrameStating("Instructions for bob.", scriptedLifetimeMs, LatestProtocolVersion),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)}
	c := New(transport, testClientInfo())
	ctx := context.Background()

	transport.become("alice")
	if instructions, err := c.Instructions(ctx); err != nil || instructions != "Instructions for alice." {
		t.Fatalf("instructions = %q err=%v", instructions, err)
	}
	if _, err := c.Tools(ctx); err != nil {
		t.Fatalf("tools: %v", err)
	}

	transport.become("bob")
	if record := c.DiscoverResult(); record != nil {
		t.Fatalf("discover result after becoming bob = %+v, want none", record)
	}
	result, err := c.CallTool(ctx, "execute_sql", region())
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	if instructions, err := c.Instructions(ctx); err != nil || instructions != "Instructions for bob." {
		t.Fatalf("instructions = %q err=%v", instructions, err)
	}

	want := []string{
		"alice server/discover", "alice tools/list",
		"bob server/discover", "bob tools/list", "bob tools/call",
	}
	if got := transport.wire(); !slices.Equal(got, want) {
		t.Fatalf("wire = %v, want %v", got, want)
	}
	if got := transport.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1: bob's call was mirrored from alice's definition", got)
	}
	if connects, disconnects, connected := transport.lifecycle(); connects != 2 || disconnects != 1 || !connected {
		t.Fatalf("connects = %d, disconnects = %d, connected = %v: want alice's channel torn down and bob's open",
			connects, disconnects, connected)
	}
}

// TestATransportWithoutTheHookHasOneContext asserts a transport that presents
// no credential of its own is left exactly as it was: there is one context, so
// nothing is ever given up and what was settled is shown.
func TestATransportWithoutTheHookHasOneContext(t *testing.T) {
	c, s := discoveryClient(t, toolsFrame(plainTool("execute_sql")), toolCallFrame("done"))
	ctx := context.Background()

	if _, err := c.Tools(ctx); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if _, err := c.CallTool(ctx, "execute_sql", region()); err != nil {
		t.Fatalf("call: %v", err)
	}
	if record := c.DiscoverResult(); record == nil || record.Instructions != "Be nice." {
		t.Fatalf("discover result = %+v, want the one the connection was settled with", record)
	}
	want := []string{"server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if connects, disconnects, _ := s.lifecycle(); connects != 1 || disconnects != 0 {
		t.Fatalf("connects = %d, disconnects = %d, want the one channel left alone", connects, disconnects)
	}
}

// TestCallersOfSeveralIdentitiesNeverShareADefinition drives one client from
// several goroutines while the token it presents keeps changing between two
// callers, under the race detector. Whatever the interleaving, every call on
// the wire carries the headers the definition advertised to its own token asks
// for: alice's definition mirrors nothing and everyone else's mirrors the
// region, so a call mirrored from the other caller's definition shows at once.
func TestCallersOfSeveralIdentitiesNeverShareADefinition(t *testing.T) {
	endpoint := newIdentityEndpoint(t)
	var asked atomic.Int64
	client := Web(endpoint.URL).WithTokenFunc(func() string {
		if asked.Add(1)%5 == 0 {
			return "bob"
		}
		return "alice"
	})

	var wg sync.WaitGroup
	failures := make(chan string, 64)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				// A call whose token changed under it once too often is refused
				// by the client itself, with nothing sent: that is the client
				// declining to guess, not a definition crossing over.
				if _, err := client.CallTool(context.Background(), "execute_sql", region()); err != nil && !isStaleDefinition(err) {
					failures <- err.Error()
				}
				if _, err := client.Instructions(context.Background()); err != nil {
					failures <- err.Error()
				}
				_ = client.DiscoverResult()
				_ = client.ExcludedTools()
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("%s", failure)
	}

	for index, call := range endpoint.requestsFor("tools/call") {
		mirrored := call.headers.Get("Mcp-Param-Region") != ""
		if who := identityOf(call); (who == "alice") == mirrored {
			t.Fatalf("call %d under the token of %s carried Mcp-Param-Region = %q", index, who, call.headers.Get("Mcp-Param-Region"))
		}
	}
}

// Ways a caller is given nothing to mirror a call from.
const (
	nothingAdvertised = "a catalogue that does not advertise the tool"
	listingRefused    = "a listing refused on protocol terms"
	nothingMirrored   = "a revision that mirrors nothing into headers"
)

// newUnequalEndpoint starts an endpoint in front of which callers are not served
// alike. The caller "alice" is given nothing to mirror a call from, in the way
// withheld names, and told is run the moment she has been: between that answer
// and the call it was read for. Every other caller is served by a discovery-era
// backend that advertises the tool mirroring its region, does not speak the
// initialize handshake, and is fronted by something that routes on the header:
// a call that arrives without it is turned away on HTTP's terms.
func newUnequalEndpoint(t *testing.T, withheld string, told func()) *recordingEndpoint {
	t.Helper()
	return newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
		who := identityOf(request)
		methodNotFound := func() {
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"error": map[string]any{
				"code": -32601, "message": "Method not found.",
			}}))
		}
		switch {
		case request.httpMethod == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case who == "alice" && withheld == nothingMirrored:
			// Her backend predates discovery, and the handshake is over once the
			// client has announced it.
			switch {
			case request.method == "initialize":
				w.Header().Set(sessionHeader, "session-of-alice")
				writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
					"protocolVersion": ProtocolV20251125,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "Server of alice", "version": "1.0.0"},
				}}))
			case len(request.id) == 0:
				told()
				w.WriteHeader(http.StatusAccepted)
			default:
				methodNotFound()
			}
		case request.method == "server/discover":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"supportedVersions": []any{LatestProtocolVersion},
				"capabilities":      map[string]any{},
				"ttlMs":             3600000,
				"cacheScope":        "private",
			}}))
		case request.method == "tools/list" && who == "alice":
			told()
			if withheld == listingRefused {
				methodNotFound()
				return
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete", "tools": []any{}, "ttlMs": 3600000, "cacheScope": "private",
			}}))
		case request.method == "tools/list":
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete", "tools": []any{regionMirroringTool()}, "ttlMs": 3600000, "cacheScope": "private",
			}}))
		case request.method == "tools/call":
			if request.headers.Get("Mcp-Param-Region") == "" {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "no route for a request without a region")
				return
			}
			writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
				"resultType": "complete",
				"content":    []any{map[string]any{"type": "text", "text": "done for " + who}},
				"isError":    false,
			}}))
		default:
			methodNotFound()
		}
	})
}

// TestHavingNothingToMirrorIsTheAnswerOfOneTokenAlone asserts what one caller
// was not given settles no other caller's call. That a catalogue does not
// advertise a tool, that the listing was refused, and that the connection was
// settled on a revision with no mirrored headers are each the server's answer
// to the token that asked, exactly as a definition is: the caller behind the
// next token may be advertised the tool mirroring a parameter. The token
// changes between the answer and the call it was read for, so a call sent on
// the strength of that answer reaches the second caller's intermediary without
// the header it routes on, which shows on the wire and fails the call.
func TestHavingNothingToMirrorIsTheAnswerOfOneTokenAlone(t *testing.T) {
	overDiscovery := []string{
		"alice server/discover", "alice tools/list",
		"bob server/discover", "bob tools/list", "bob tools/call",
	}
	tests := []struct {
		name     string
		wantWire []string
	}{
		{name: nothingAdvertised, wantWire: overDiscovery},
		{name: listingRefused, wantWire: overDiscovery},
		{name: nothingMirrored, wantWire: []string{
			"alice server/discover", "alice initialize", "alice notifications/initialized",
			"alice DELETE", "bob initialize", "bob server/discover", "bob tools/list", "bob tools/call",
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var current atomic.Value
			current.Store("alice")
			endpoint := newUnequalEndpoint(t, tc.name, func() { current.Store("bob") })
			client := Web(endpoint.URL).WithTokenFunc(func() string { return current.Load().(string) })

			result, err := client.CallTool(context.Background(), "execute_sql", region())
			if err != nil {
				t.Fatalf("call: %v (wire = %v)", err, endpoint.wire())
			}
			if result.Text() != "done for bob" {
				t.Fatalf("text = %q, want done for bob", result.Text())
			}
			if got := endpoint.wire(); !slices.Equal(got, tc.wantWire) {
				t.Fatalf("wire = %v, want %v", got, tc.wantWire)
			}
			calls := endpoint.requestsFor("tools/call")
			if len(calls) != 1 {
				t.Fatalf("tools/call was sent %d times, want once", len(calls))
			}
			if got := calls[0].headers.Get("Mcp-Param-Region"); got != "us-west1" {
				t.Fatalf("bob's call carried Mcp-Param-Region = %q, want us-west1: it was sent on what alice was not given", got)
			}
		})
	}
}

// TestACallIsNotSentOnAnAnswerItsTokenWasNotGiven asserts the call a client
// falls back to, once the definition it held is out of date and the catalogue
// read again has none to give, is held to the connection that had none to give.
// The token changes twice under one call: the definition read for alice is out
// of date by the time the call would travel under bob's token, bob is given
// nothing to mirror from, and the token is carol's by the time the call would
// be sent with what the caller stated. Carol is advertised the tool mirroring
// its region, so that call would reach her intermediary without the header. It
// is refused by the client instead, with nothing sent: a client that declines
// to guess once more, rather than one that reads the catalogue for ever.
func TestACallIsNotSentOnAnAnswerItsTokenWasNotGiven(t *testing.T) {
	for _, withheld := range []string{nothingAdvertised, listingRefused} {
		t.Run(withheld, func(t *testing.T) {
			var current atomic.Value
			current.Store("alice")
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				who := identityOf(request)
				listed := func(tools ...any) {
					// A catalogue of nothing is still an array on the wire.
					entries := append([]any{}, tools...)
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"resultType": "complete", "tools": entries, "ttlMs": 3600000, "cacheScope": "private",
					}}))
				}
				switch {
				case request.method == "server/discover":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"supportedVersions": []any{LatestProtocolVersion},
						"capabilities":      map[string]any{},
						"ttlMs":             3600000,
						"cacheScope":        "private",
					}}))
				case request.method == "tools/list" && who == "alice":
					current.Store("bob")
					listed(unmirroredTool())
				case request.method == "tools/list" && who == "bob":
					current.Store("carol")
					if withheld == listingRefused {
						writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"error": map[string]any{
							"code": -32601, "message": "Method not found.",
						}}))
						return
					}
					listed()
				case request.method == "tools/list":
					listed(regionMirroringTool())
				default:
					if who == "carol" && request.headers.Get("Mcp-Param-Region") == "" {
						w.Header().Set("Content-Type", "text/plain")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, "no route for a request without a region")
						return
					}
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"resultType": "complete",
						"content":    []any{map[string]any{"type": "text", "text": "done for " + who}},
						"isError":    false,
					}}))
				}
			})
			client := Web(endpoint.URL).WithTokenFunc(func() string { return current.Load().(string) })

			result, err := client.CallTool(context.Background(), "execute_sql", region())
			if err == nil {
				t.Fatalf("call = %q, want it refused: no caller was given a definition to send it on", result.Text())
			}
			if !strings.Contains(err.Error(), "no longer stands") {
				t.Fatalf("error = %q, want the client's own refusal to send", err.Error())
			}
			if calls := endpoint.requestsFor("tools/call"); len(calls) != 0 {
				t.Fatalf("tools/call reached the wire under the token of %s with Mcp-Param-Region = %q",
					identityOf(calls[0]), calls[0].headers.Get("Mcp-Param-Region"))
			}
			want := []string{
				"alice server/discover", "alice tools/list",
				"bob server/discover", "bob tools/list", "carol server/discover",
			}
			if got := endpoint.wire(); !slices.Equal(got, want) {
				t.Fatalf("wire = %v, want %v", got, want)
			}
		})
	}
}

// TestLivenessIsAskedOfTheTokenThatAsked asserts a liveness check is one
// exchange, under one token, named for the connection it travels over. The
// callers are served by backends of different revisions and the token changes
// once the first caller's handshake is done. A check that settled a connection,
// read its revision, and only then sent the request that revision defines would
// send it under the second token, over the connection that token settles, to a
// backend that does not define it: a live server reported as not answering.
func TestLivenessIsAskedOfTheTokenThatAsked(t *testing.T) {
	tests := []struct {
		name string
		// legacy is the caller whose backend predates discovery; the other's
		// speaks nothing else.
		legacy   string
		wantWire []string
	}{
		{
			name:   "from a backend that defines ping to one that removed it",
			legacy: "alice",
			wantWire: []string{
				"alice server/discover", "alice initialize", "alice notifications/initialized", "alice ping",
			},
		},
		{
			name:     "from a backend that removed ping to one that defines it",
			legacy:   "bob",
			wantWire: []string{"alice server/discover", "alice server/discover"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var current atomic.Value
			current.Store("alice")
			endpoint := newRecordingEndpoint(t, func(w http.ResponseWriter, request recordedRequest) {
				who := identityOf(request)
				methodNotFound := func() {
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"error": map[string]any{
						"code": -32601, "message": "Method not found.",
					}}))
				}
				switch {
				case request.httpMethod == http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				case who == tc.legacy && request.method == "initialize":
					w.Header().Set(sessionHeader, "session-of-"+who)
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"protocolVersion": ProtocolV20251125,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "Server of " + who, "version": "1.0.0"},
					}}))
				case who == tc.legacy && len(request.id) == 0:
					current.Store("bob")
					w.WriteHeader(http.StatusAccepted)
				case who == tc.legacy && request.method == "ping":
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{}}))
				case who != tc.legacy && request.method == "server/discover":
					current.Store("bob")
					writeJSON(w, http.StatusOK, jsonFrame(request.id, map[string]any{"result": map[string]any{
						"supportedVersions": []any{LatestProtocolVersion},
						"capabilities":      map[string]any{},
						"ttlMs":             3600000,
						"cacheScope":        "private",
					}}))
				default:
					methodNotFound()
				}
			})
			client := Web(endpoint.URL).WithTokenFunc(func() string { return current.Load().(string) })

			if err := client.Ping(context.Background()); err != nil {
				t.Fatalf("ping: %v (wire = %v)", err, endpoint.wire())
			}
			if got := endpoint.wire(); !slices.Equal(got, tc.wantWire) {
				t.Fatalf("wire = %v, want %v", got, tc.wantWire)
			}
		})
	}
}
