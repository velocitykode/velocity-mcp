package server_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// The caching hints are asserted as the raw JSON members they went out as,
// because both are wire contracts a client parses by type: "ttlMs" is an
// integer number of milliseconds and "cacheScope" is one of two strings.
// Decoding them into map[string]any first would turn the integer into a float
// and hide, for instance, a lifetime emitted as 3e+05.

// resultMembers decodes a successful result into its raw JSON members.
func resultMembers(t *testing.T, resp *jsonrpc.Response) map[string]json.RawMessage {
	t.Helper()
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &members); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return members
}

// assertCacheHints fails unless the result carries exactly the given hints.
func assertCacheHints(t *testing.T, members map[string]json.RawMessage, wantTTL, wantScope string) {
	t.Helper()
	if got := string(members["ttlMs"]); got != wantTTL {
		t.Fatalf("ttlMs = %s, want %s", orAbsent(got), wantTTL)
	}
	if got := string(members["cacheScope"]); got != wantScope {
		t.Fatalf("cacheScope = %s, want %s", orAbsent(got), wantScope)
	}
}

// assertNoCacheHints fails unless the result carries neither hint.
func assertNoCacheHints(t *testing.T, members map[string]json.RawMessage) {
	t.Helper()
	if _, ok := members["ttlMs"]; ok {
		t.Fatalf("ttlMs = %s, want it absent", members["ttlMs"])
	}
	if _, ok := members["cacheScope"]; ok {
		t.Fatalf("cacheScope = %s, want it absent", members["cacheScope"])
	}
}

func orAbsent(raw string) string {
	if raw == "" {
		return "absent"
	}
	return raw
}

// cachedDocResource prices its own freshness, as a resource whose contents are
// the same for every caller and change slowly does.
type cachedDocResource struct{}

func (cachedDocResource) Name() string        { return "cached-doc" }
func (cachedDocResource) Description() string { return "A document with its own freshness" }
func (cachedDocResource) URI() string         { return "file://cached.txt" }
func (cachedDocResource) MimeType() string    { return "text/plain" }
func (cachedDocResource) Read(context.Context, *server.Request) (*server.Response, error) {
	return server.Text("cached"), nil
}
func (cachedDocResource) CacheHint() server.CacheHint {
	return server.CacheHint{TTL: 2 * time.Minute, Scope: server.CacheScopePublic}
}

// cachedUserTemplate is the templated counterpart: the hint has to be found
// through a template match, not only through an exact URI.
type cachedUserTemplate struct{}

func (cachedUserTemplate) Name() string        { return "cached-user" }
func (cachedUserTemplate) Description() string { return "A user record" }
func (cachedUserTemplate) URI() string         { return "file://users/{id}" }
func (cachedUserTemplate) URITemplate() string { return "file://users/{id}" }
func (cachedUserTemplate) MimeType() string    { return "text/plain" }
func (cachedUserTemplate) CacheHint() server.CacheHint {
	return server.CacheHint{TTL: 30 * time.Second}
}
func (cachedUserTemplate) Read(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("user:" + req.String("id")), nil
}

// cachingServer registers one primitive of every kind the cacheable operations
// report, including a resource that prices itself and one that does not.
func cachingServer(opts ...server.Option) *server.Server {
	base := []server.Option{
		server.WithTools(addTool()),
		server.WithPrompts(greetPrompt{}),
		server.WithResources(docResource{}, cachedDocResource{}, cachedUserTemplate{}),
	}
	return server.New("demo", "1.0.0", append(base, opts...)...)
}

// TestCachingHintsOnEveryCacheableResult asserts the six operations whose
// complete results are cacheable all carry the two caching members, and that a
// server that configured nothing advertises the conservative pair: stale on
// arrival, never shared across authorization contexts.
func TestCachingHintsOnEveryCacheableResult(t *testing.T) {
	tests := []struct{ method, extra string }{
		{"server/discover", ""},
		{"tools/list", ""},
		{"prompts/list", ""},
		{"resources/list", ""},
		{"resources/templates/list", ""},
		{"resources/read", `"uri":"file://doc.txt"`},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			raw := modernRequest(1, tt.method)
			if tt.extra != "" {
				raw = modernRequest(1, tt.method, tt.extra)
			}
			members := resultMembers(t, handle(t, cachingServer(), raw).Response)
			if got := string(members["resultType"]); got != `"complete"` {
				t.Fatalf("resultType = %s, want complete", orAbsent(got))
			}
			assertCacheHints(t, members, "0", `"private"`)
		})
	}
}

// TestResultsThatAreNotCacheableCarryNoHints asserts the hints stay off every
// other result. A tool call is not cacheable at all, so advertising a lifetime
// for it would invite a client to serve a stale side effect.
func TestResultsThatAreNotCacheableCarryNoHints(t *testing.T) {
	tests := []struct{ name, raw string }{
		{"tools/call", modernRequest(1, "tools/call", `"name":"add","arguments":{"a":1,"b":2}`)},
		{"prompts/get", modernRequest(1, "prompts/get", `"name":"greet"`)},
		{"ping", modernRequest(1, "ping")},
		{"initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`},
		{"legacy tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			members := resultMembers(t, handle(t, cachingServer(), tt.raw).Response)
			if tt.name == "legacy tools/list" {
				// A cacheable operation stays cacheable whichever handshake
				// reached it; this case only pins that the hints do not depend
				// on the discovery metadata being present.
				assertCacheHints(t, members, "0", `"private"`)
				return
			}
			assertNoCacheHints(t, members)
		})
	}
}

// TestCacheHintsAreNeutralisedOnARetriedRequest asserts a request replayed with
// client input is never priced for reuse: its result depends on inputs that are
// not part of the cache key, so the widest configured hint is still answered
// with a lifetime of zero under private scope.
//
// The members stay present rather than being dropped, because a complete result
// of a cacheable operation is required to carry them; zero and private is how
// the protocol says "do not reuse this".
func TestCacheHintsAreNeutralisedOnARetriedRequest(t *testing.T) {
	tests := []struct{ name, extra string }{
		{"inputResponses", `"uri":"file://cached.txt","inputResponses":{"login":{"action":"accept"}}`},
		{"requestState", `"uri":"file://cached.txt","requestState":"opaque-token"`},
		{"null requestState", `"uri":"file://cached.txt","requestState":null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := cachingServer(server.WithCacheHint(server.CacheHint{
				TTL:   time.Hour,
				Scope: server.CacheScopePublic,
			}))
			members := resultMembers(t, handle(t, s, modernRequest(1, "resources/read", tt.extra)).Response)
			assertCacheHints(t, members, "0", `"private"`)
		})
	}
}

// TestServerWideCacheHintIsAdvertised asserts a configured hint reaches every
// cacheable operation, in the unit and spelling the wire defines.
func TestServerWideCacheHintIsAdvertised(t *testing.T) {
	s := cachingServer(server.WithCacheHint(server.CacheHint{
		TTL:   5 * time.Minute,
		Scope: server.CacheScopePublic,
	}))
	for _, method := range []string{"server/discover", "tools/list", "prompts/list", "resources/list", "resources/templates/list"} {
		t.Run(method, func(t *testing.T) {
			members := resultMembers(t, handle(t, s, modernRequest(1, method)).Response)
			assertCacheHints(t, members, "300000", `"public"`)
		})
	}
}

// TestMethodCacheHintOverridesTheServerHint asserts the per-operation hint wins
// where it is set and leaves every other operation on the server-wide one.
func TestMethodCacheHintOverridesTheServerHint(t *testing.T) {
	s := cachingServer(
		server.WithCacheHint(server.CacheHint{TTL: 5 * time.Minute, Scope: server.CacheScopePublic}),
		server.WithMethodCacheHint("tools/list", server.CacheHint{TTL: time.Minute}),
	)

	tools := resultMembers(t, handle(t, s, modernRequest(1, "tools/list")).Response)
	assertCacheHints(t, tools, "60000", `"private"`)

	prompts := resultMembers(t, handle(t, s, modernRequest(2, "prompts/list")).Response)
	assertCacheHints(t, prompts, "300000", `"public"`)
}

// TestResourceCacheHintOverridesTheServerHints asserts a resource that prices
// itself wins over both the per-operation hint and the server-wide one, that a
// resource which does not falls back to them, and that a templated resource is
// found through a template match rather than only by an exact URI.
func TestResourceCacheHintOverridesTheServerHints(t *testing.T) {
	s := cachingServer(
		server.WithCacheHint(server.CacheHint{TTL: 5 * time.Minute, Scope: server.CacheScopePublic}),
		server.WithMethodCacheHint("resources/read", server.CacheHint{TTL: time.Minute}),
	)

	tests := []struct{ name, uri, wantTTL, wantScope string }{
		{"resource with its own hint", "file://cached.txt", "120000", `"public"`},
		{"template matched concretely", "file://users/7", "30000", `"private"`},
		{"template as written", "file://users/{id}", "30000", `"private"`},
		{"resource without one", "file://doc.txt", "60000", `"private"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := modernRequest(1, "resources/read", `"uri":`+mustQuote(tt.uri))
			members := resultMembers(t, handle(t, s, raw).Response)
			assertCacheHints(t, members, tt.wantTTL, tt.wantScope)
		})
	}
}

// mustQuote renders a string as a JSON string literal.
func mustQuote(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// TestCacheHintRendering drives the values a hint can be configured with
// through the wire. The lifetime is an integer count of milliseconds that is
// never negative, and any scope but an explicit public one is reported as
// private, so neither a zero value nor a typo can widen who may reuse a result.
func TestCacheHintRendering(t *testing.T) {
	tests := []struct {
		name      string
		hint      server.CacheHint
		wantTTL   string
		wantScope string
	}{
		{"zero value", server.CacheHint{}, "0", `"private"`},
		{"negative lifetime", server.CacheHint{TTL: -time.Hour}, "0", `"private"`},
		{"sub-millisecond lifetime", server.CacheHint{TTL: 999 * time.Microsecond}, "0", `"private"`},
		{"one millisecond", server.CacheHint{TTL: time.Millisecond}, "1", `"private"`},
		{"rounds down", server.CacheHint{TTL: 1500*time.Millisecond + 999*time.Microsecond}, "1500", `"private"`},
		{"five minutes public", server.CacheHint{TTL: 5 * time.Minute, Scope: server.CacheScopePublic}, "300000", `"public"`},
		{"explicit private", server.CacheHint{TTL: time.Second, Scope: server.CacheScopePrivate}, "1000", `"private"`},
		{"unknown scope narrows", server.CacheHint{TTL: time.Second, Scope: server.CacheScope("PUBLIC")}, "1000", `"private"`},
		{"empty scope narrows", server.CacheHint{TTL: time.Second, Scope: server.CacheScope("")}, "1000", `"private"`},
		{"a day", server.CacheHint{TTL: 24 * time.Hour, Scope: server.CacheScopePublic}, "86400000", `"public"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := cachingServer(server.WithCacheHint(tt.hint))
			members := resultMembers(t, handle(t, s, modernRequest(1, "tools/list")).Response)
			assertCacheHints(t, members, tt.wantTTL, tt.wantScope)
		})
	}
}

// pricedMethod is a handler that prices its own result, as a method that knows
// more about its data than the server-wide configuration does.
type pricedMethod struct{}

func (pricedMethod) Handle(_ *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, map[string]any{
		"tools":      []any{},
		"ttlMs":      42,
		"cacheScope": "public",
	})
}

// TestHandlerCacheHintsSurviveTheEnvelope asserts the envelope supplies the
// hints rather than imposing them: a handler that wrote its own keeps them.
func TestHandlerCacheHintsSurviveTheEnvelope(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithCacheHint(server.CacheHint{TTL: time.Hour}),
		server.WithMethod("tools/list", pricedMethod{}),
	)
	members := resultMembers(t, handle(t, s, modernRequest(1, "tools/list")).Response)
	assertCacheHints(t, members, "42", `"public"`)
}

// TestInterimResultsCarryNoCachingHints asserts a call the server suspended
// awaiting client input is not priced. Only a complete result is cacheable, so
// an interim one carries no hints whatever the operation is configured with.
func TestInterimResultsCarryNoCachingHints(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithCacheHint(server.CacheHint{TTL: time.Hour, Scope: server.CacheScopePublic}),
		server.WithMethod("tools/list", opinionatedMethod{}),
	)
	members := resultMembers(t, handle(t, s, modernRequest(1, "tools/list")).Response)
	if got := string(members["resultType"]); got != `"input_required"` {
		t.Fatalf("resultType = %s, want input_required", orAbsent(got))
	}
	assertNoCacheHints(t, members)
}

// malformedResultTypeMethod states a result type that is not a string, which
// the protocol does not use but a handler with a bug can produce.
type malformedResultTypeMethod struct{}

func (malformedResultTypeMethod) Handle(_ *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, map[string]any{"tools": []any{}, "resultType": 7})
}

// TestAResultTypeThatIsNotCompleteCarriesNoCachingHints asserts the hints
// follow the result type rather than the method name: a result the envelope
// cannot read as complete is not priced, because a client that cached it would
// be caching something the server never said was finished.
func TestAResultTypeThatIsNotCompleteCarriesNoCachingHints(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithCacheHint(server.CacheHint{TTL: time.Hour, Scope: server.CacheScopePublic}),
		server.WithMethod("tools/list", malformedResultTypeMethod{}),
	)
	members := resultMembers(t, handle(t, s, modernRequest(1, "tools/list")).Response)
	if got := string(members["resultType"]); got != "7" {
		t.Fatalf("resultType = %s, want the handler's own value", orAbsent(got))
	}
	assertNoCacheHints(t, members)
}

// TestPagesOfOneListShareTheCacheScope asserts every page of a paginated list
// carries the same scope, which a client relies on when it caches pages
// independently: a first page that may be shared followed by one that may not
// would leak the second through the cache of the first.
func TestPagesOfOneListShareTheCacheScope(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithCacheHint(server.CacheHint{TTL: time.Minute, Scope: server.CacheScopePublic}),
		server.WithPageSize(1),
		server.WithTools(addTool(), server.NewTool("second", "Another tool").
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("ok"), nil
			})),
	)

	first := resultMembers(t, handle(t, s, modernRequest(1, "tools/list")).Response)
	assertCacheHints(t, first, "60000", `"public"`)

	cursor := string(first["nextCursor"])
	if cursor == "" {
		t.Fatalf("the first page carries no cursor: %v", first)
	}
	second := resultMembers(t, handle(t, s, modernRequest(2, "tools/list", `"cursor":`+cursor)).Response)
	assertCacheHints(t, second, "60000", `"public"`)
}

// TestUnresolvableReadIsRefusedRatherThanPriced asserts a read of a uri no
// resource answers stays an error response: an error is not a result, so it
// carries no hints for a client to cache.
func TestUnresolvableReadIsRefusedRatherThanPriced(t *testing.T) {
	s := cachingServer(server.WithCacheHint(server.CacheHint{TTL: time.Hour}))
	res := handle(t, s, modernRequest(1, "resources/read", `"uri":"file://nope"`))
	if code := errorOf(t, res).Code; code != jsonrpc.CodeInvalidParams {
		t.Fatalf("code = %d, want invalid params", code)
	}
	if len(res.Response.Result) != 0 {
		t.Fatalf("an error response must carry no result, got %s", res.Response.Result)
	}
}

// TestHostileParamsDoNotBreakTheCachingHints drives params the resolution has
// to read through shapes a client can send but the schema does not describe. A
// bag it cannot make sense of must still produce a well-formed result: the
// hints fall back to the configured ones rather than the server failing to
// answer.
func TestHostileParamsDoNotBreakTheCachingHints(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantTTL   string
		wantScope string
	}{
		{
			name:      "uri that is not a string",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + protocolMeta + `,"uri":7}}`,
			wantTTL:   "60000",
			wantScope: `"public"`,
		},
		{
			// Refused before any handler runs, so there is no result to price.
			name: "params that are not an object",
			raw:  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[1,2,3]}`,
		},
		{
			name:      "no params at all",
			raw:       `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			wantTTL:   "60000",
			wantScope: `"public"`,
		},
		{
			name:      "a control character in the uri",
			raw:       modernRequest(1, "resources/read", `"uri":`+mustQuote("file://doc.txt\x00")),
			wantTTL:   "",
			wantScope: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := cachingServer(server.WithCacheHint(server.CacheHint{
				TTL:   time.Minute,
				Scope: server.CacheScopePublic,
			}))
			res := handle(t, s, tt.raw)
			if tt.wantTTL == "" {
				// The request is refused, so there is no result to price and
				// nothing for a client to cache.
				if res.Response.Error == nil {
					t.Fatalf("expected an error response, got %s", res.Response.Result)
				}
				if len(res.Response.Result) != 0 {
					t.Fatalf("an error response must carry no result, got %s", res.Response.Result)
				}
				return
			}
			assertCacheHints(t, resultMembers(t, res.Response), tt.wantTTL, tt.wantScope)
		})
	}
}
