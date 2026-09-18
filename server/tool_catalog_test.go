package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/velocitykode/velocity/validation"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/event"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
	_ "github.com/velocitykode/velocity-mcp/server/methods" // installs the full method set
)

// The generic message a contained failure reports to the client, identical to
// the one a direct tools/call returns for the same failure.
const internalFailureText = "Something went wrong while processing the request."

// sayHiTool is the catalog fixture used across the tool catalog tests: it
// validates its single argument so the inner-validation path is exercised.
func sayHiTool() server.Tool {
	return server.NewTool("say-hi-tool", "This tool says hello to a person").
		WithSchema(func(s *schema.Object) {
			s.String("name").Description("The name of the person to greet").Required()
		}).
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			if err := req.Validate(validation.Rules{
				"name": {validation.Required(), validation.String()},
			}); err != nil {
				return nil, err
			}
			return server.Text("Hello, " + req.String("name") + "!"), nil
		})
}

// structuredTool returns structured content alongside its text, so the tests can
// assert that a batch result carries the same structuredContent a direct
// tools/call would.
func structuredTool() server.Tool {
	return server.NewTool("structured-content-tool", "Reports the current weather").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.JSON(map[string]any{
				"temperature": 22.5,
				"conditions":  "Partly cloudy",
				"humidity":    65,
			})
		})
}

// recordingTool records every invocation so a test can prove a call was never
// reached. It is safe for concurrent use.
type recordingTool struct {
	mu     sync.Mutex
	values []string
}

func (r *recordingTool) Name() string        { return "recording-tool" }
func (r *recordingTool) Description() string { return "Records the values it is given" }
func (r *recordingTool) Schema(s *schema.Object) {
	s.String("value").Required()
}

func (r *recordingTool) Handle(_ context.Context, req *server.Request) (*server.Response, error) {
	r.mu.Lock()
	r.values = append(r.values, req.String("value"))
	r.mu.Unlock()
	return server.Text(req.String("value")), nil
}

func (r *recordingTool) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.values...)
}

// failingTool returns a non-validation error, the case the catalog must contain
// without leaking the underlying message.
func failingTool() server.Tool {
	return server.NewTool("failing-tool", "Always fails").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return nil, fmt.Errorf("connection to 10.0.0.5:5432 refused: secret-token")
		})
}

// rpcFailingTool returns a protocol error, which a direct call turns into a
// JSON-RPC error response carrying that message.
func rpcFailingTool() server.Tool {
	return server.NewTool("denied-tool", "Always refuses").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "Access to this tool is denied.")
		})
}

// echoTool returns its single argument verbatim, so tests can size a result
// precisely.
func echoTool() server.Tool {
	return server.NewTool("echo-tool", "Echoes the value it is given").
		WithSchema(func(s *schema.Object) { s.String("value") }).
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			return server.Text(req.String("value")), nil
		})
}

// progressTool reports progress before returning, so the test can prove inner
// progress notifications reach the client during a batch.
func progressTool() server.Tool {
	return server.NewTool("progress-tool", "Reports progress while working").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			for i := 1; i <= 2; i++ {
				if err := req.ReportProgress(server.ProgressUpdate{Progress: float64(i), Total: 2}); err != nil {
					return nil, err
				}
			}
			return server.Text("done"), nil
		})
}

// catalogReply is one meta-tool reply: the bytes the server put on the wire for
// it, and the decoded views the assertions read.
type catalogReply struct {
	// wire is the result member of the response frame, exactly as it was
	// serialized. Budget measurements are taken from it, never from a
	// re-encoding of the decoded form.
	wire    json.RawMessage
	result  map[string]any
	payload map[string]any
	text    string
}

// callCatalog drives a tools/call for the named tool through the server and
// returns the reply it produced.
func callCatalog(t *testing.T, s *server.Server, name, argumentsJSON string) catalogReply {
	t.Helper()
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, argumentsJSON)
	res := handle(t, s, raw)
	reply := catalogReply{result: decodeResult(t, res.Response)}
	reply.wire = res.Response.Result
	reply.text = firstText(t, reply.result)
	if reply.text != "" && (strings.HasPrefix(reply.text, "{") || strings.HasPrefix(reply.text, "[")) {
		if err := json.Unmarshal([]byte(reply.text), &reply.payload); err != nil {
			reply.payload = nil
		}
	}
	return reply
}

// callCatalogTool drives a tools/call for the named tool through the server and
// returns the decoded tool result, the JSON payload carried in its first text
// content item, and the raw text.
func callCatalogTool(t *testing.T, s *server.Server, name, argumentsJSON string) (result map[string]any, payload map[string]any, text string) {
	t.Helper()
	reply := callCatalog(t, s, name, argumentsJSON)
	return reply.result, reply.payload, reply.text
}

// firstText returns the text of a tool result's first content item.
func firstText(t *testing.T, result map[string]any) string {
	t.Helper()
	items, ok := result["content"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("result carries no content: %#v", result)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("content item is not an object: %#v", items[0])
	}
	if got := item["type"]; got != "text" {
		t.Fatalf("content type = %v, want text", got)
	}
	text, _ := item["text"].(string)
	return text
}

// batchContent returns the content items an execute_tools reply forwarded from
// the calls it ran: every item after the report that leads the content.
func batchContent(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	items, ok := result["content"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("result carries no content: %#v", result)
	}
	forwarded := make([]map[string]any, 0, len(items)-1)
	for _, raw := range items[1:] {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("content item is not an object: %#v", raw)
		}
		forwarded = append(forwarded, item)
	}
	return forwarded
}

// batchResults reassembles the "tools/call" result of every call an
// execute_tools reply reports: each record takes its share of the forwarded
// content items, in call order, using the item count the record states. It is
// the reading a client performs, so a report whose counts do not match the
// items the reply carries fails here.
func batchResults(t *testing.T, result, payload map[string]any) []map[string]any {
	t.Helper()
	items := batchContent(t, result)
	records, ok := payload["results"].([]any)
	if !ok {
		t.Fatalf("payload has no results array: %#v", payload)
	}

	out := make([]map[string]any, 0, len(records))
	at := 0
	for _, raw := range records {
		record, _ := raw.(map[string]any)
		count, ok := record["contentItems"].(float64)
		if !ok {
			t.Fatalf("record states no content item count: %s", mustJSON(record))
		}
		if at+int(count) > len(items) {
			t.Fatalf("record %s claims %v items, %d are left", mustJSON(record), count, len(items)-at)
		}
		content := make([]any, 0, int(count))
		for _, item := range items[at : at+int(count)] {
			content = append(content, item)
		}
		at += int(count)

		call := map[string]any{"content": content, "isError": record["isError"]}
		if structured, ok := record["structuredContent"]; ok {
			call["structuredContent"] = structured
		}
		out = append(out, call)
	}
	if at != len(items) {
		t.Fatalf("the reply carries %d forwarded content items, the report accounts for %d", len(items), at)
	}
	return out
}

// replySize returns how many bytes of the response frame the meta tool itself
// produced: the serialized result with the members the reply envelope adds
// afterwards taken back out. The members that remain are carried over as the
// raw bytes the server wrote, never re-encoded from a decoded value, so a
// budget derived from this is the size of what really went on the wire,
// escaping included.
func replySize(t *testing.T, wire json.RawMessage) int {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(wire, &members); err != nil {
		t.Fatalf("decode result frame: %v", err)
	}

	for key := range members {
		switch key {
		case "content", "isError", "structuredContent", "_meta":
		case "resultType", "ttlMs", "cacheScope":
			// Written by the reply envelope, not by the tool.
			delete(members, key)
		default:
			t.Fatalf("unexpected result member %q: %s", key, wire)
		}
	}

	if raw, ok := members["_meta"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("decode result metadata: %v", err)
		}
		delete(meta, server.MetaKeyServerInfo)
		if len(meta) == 0 {
			delete(members, "_meta")
		} else {
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatalf("encode result metadata: %v", err)
			}
			members["_meta"] = encoded
		}
	}

	encoded, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("encode result frame: %v", err)
	}
	return len(encoded)
}

// assertWithinBudget fails when a reply is larger than the budget it was served
// under.
func assertWithinBudget(t *testing.T, reply catalogReply, budget int) {
	t.Helper()
	if size := replySize(t, reply.wire); size > budget {
		t.Fatalf("the reply is %d bytes, past the %d byte budget it announces: %s", size, budget, reply.wire)
	}
}

// measureReply runs one catalog call under a budget wide enough to carry it and
// returns the size of the reply that came back.
func measureReply(t *testing.T, tools []server.Tool, name, arguments string) int {
	t.Helper()
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(tools...),
		server.WithToolCatalogLimits(25, 1<<20),
	)
	reply := callCatalog(t, s, name, arguments)
	if got := reply.result["isError"]; got != false {
		t.Fatalf("the measured call failed: isError = %v (text %q)", got, reply.text)
	}
	return replySize(t, reply.wire)
}

// toolNames returns the names reported by tools/list, in order.
func toolNames(t *testing.T, s *server.Server) []string {
	t.Helper()
	result := decodeResult(t, handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Response)
	listed, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list result has no tools array: %#v", result)
	}
	names := make([]string, 0, len(listed))
	for _, entry := range listed {
		tool, _ := entry.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

// resultNames returns the tool name of each entry in an execute_tools payload.
func resultNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	entries, ok := payload["results"].([]any)
	if !ok {
		t.Fatalf("payload has no results array: %#v", payload)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		result, _ := entry.(map[string]any)
		name, _ := result["name"].(string)
		names = append(names, name)
	}
	return names
}

// searchNames returns the tool name of each entry in a search_tools payload.
func searchNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	entries, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("payload has no tools array: %#v", payload)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		tool, _ := entry.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestToolCatalogReplacesEntriesWithTwoMetaTools(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithTools(addTool()),
		server.WithToolCatalog(sayHiTool()),
		server.WithToolCatalog(structuredTool()),
	)

	if got, want := toolNames(t, s), []string{"add", "search_tools", "execute_tools"}; !equalStrings(got, want) {
		t.Fatalf("tools/list names = %v, want %v", got, want)
	}

	result := decodeResult(t, handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Response)
	listed, _ := result["tools"].([]any)
	search, _ := listed[1].(map[string]any)
	execute, _ := listed[2].(map[string]any)

	if got := search["title"]; got != "Search Tools" {
		t.Errorf("search_tools title = %v, want Search Tools", got)
	}
	if got := execute["title"]; got != "Execute Tools" {
		t.Errorf("execute_tools title = %v, want Execute Tools", got)
	}

	searchAnnotations, _ := search["annotations"].(map[string]any)
	if got := searchAnnotations["readOnlyHint"]; got != true {
		t.Errorf("search_tools readOnlyHint = %v, want true", got)
	}
	executeAnnotations, _ := execute["annotations"].(map[string]any)
	if got := executeAnnotations["openWorldHint"]; got != true {
		t.Errorf("execute_tools openWorldHint = %v, want true", got)
	}

	// The meta tools stand in for the catalog: a catalog tool is reachable
	// through execute_tools but must not be callable directly.
	res := handle(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"say-hi-tool","arguments":{"name":"Ada"}}}`)
	if res.Response == nil || res.Response.Error == nil {
		t.Fatalf("direct call to a catalog tool should fail, got %+v", res.Response)
	}
	if got, want := res.Response.Error.Code, -32602; got != want {
		t.Errorf("error code = %d, want %d", got, want)
	}
	if got, want := res.Response.Error.Message, "Tool [say-hi-tool] not found."; got != want {
		t.Errorf("error message = %q, want %q", got, want)
	}
}

func TestToolCatalogSearchReturnsCompleteEntry(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), structuredTool()))

	result, payload, _ := callCatalogTool(t, s, "search_tools", `{"query":"person name"}`)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false", got)
	}

	want := map[string]any{
		"ok": true,
		"tools": []any{map[string]any{
			"name":        "say-hi-tool",
			"description": "This tool says hello to a person",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The name of the person to greet",
					},
				},
				"required": []any{"name"},
			},
		}},
		"hasMore": false,
	}
	if !jsonEqual(payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(payload), mustJSON(want))
	}
}

func TestToolCatalogSearchIncludesAnnotations(t *testing.T) {
	destructive := server.NewTool("delete-thing-tool", "Deletes a thing").
		WithDestructiveHint(true).
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text("deleted"), nil
		})
	s := server.New("demo", "1.0.0", server.WithToolCatalog(destructive, sayHiTool()))

	_, payload, _ := callCatalogTool(t, s, "search_tools", `{"query":"delete"}`)
	entries, _ := payload["tools"].([]any)
	if len(entries) != 1 {
		t.Fatalf("tools = %s, want exactly one entry", mustJSON(payload["tools"]))
	}
	entry, _ := entries[0].(map[string]any)
	if !jsonEqual(entry["annotations"], map[string]any{"destructiveHint": true}) {
		t.Fatalf("annotations = %s, want {\"destructiveHint\":true}", mustJSON(entry["annotations"]))
	}

	// A tool that declares no hints carries no annotations key at all.
	_, payload, _ = callCatalogTool(t, s, "search_tools", `{"query":"hello"}`)
	entries, _ = payload["tools"].([]any)
	entry, _ = entries[0].(map[string]any)
	if _, present := entry["annotations"]; present {
		t.Fatalf("unannotated tool carries annotations: %s", mustJSON(entry))
	}
}

func TestToolCatalogSearchRanking(t *testing.T) {
	// alpha wins on an exact name-term query; beta names "alpha" only in its
	// description; gamma mentions it only in its schema, the lowest weight.
	alpha := server.NewTool("alpha", "First tool").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("a"), nil })
	beta := server.NewTool("beta", "Mentions alpha in the description").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("b"), nil })
	gamma := server.NewTool("gamma", "Unrelated").
		WithSchema(func(s *schema.Object) { s.String("alpha") }).
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("g"), nil })

	s := server.New("demo", "1.0.0", server.WithToolCatalog(gamma, beta, alpha))

	tests := []struct {
		name    string
		args    string
		want    []string
		hasMore bool
	}{
		{
			name: "exact name terms outrank every other signal",
			args: `{"query":"alpha"}`,
			want: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "an empty query browses the catalog in registration order",
			args: `{"query":""}`,
			want: []string{"gamma", "beta", "alpha"},
		},
		{
			name: "an omitted query browses the catalog",
			args: `{}`,
			want: []string{"gamma", "beta", "alpha"},
		},
		{
			name: "punctuation and case are term separators, not terms",
			args: `{"query":"  ALPHA!!  "}`,
			want: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "a query that matches nothing returns nothing",
			args: `{"query":"zzzz"}`,
			want: []string{},
		},
		{
			name:    "the limit truncates and reports more",
			args:    `{"query":"","limit":2}`,
			want:    []string{"gamma", "beta"},
			hasMore: true,
		},
		{
			name: "terms are matched independently",
			args: `{"query":"gamma alpha"}`,
			// gamma scores 4 (name) + 1 (its own schema mentions alpha);
			// alpha scores 4 on its name; beta scores 2 on its description.
			want: []string{"gamma", "alpha", "beta"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, payload, text := callCatalogTool(t, s, "search_tools", tc.args)
			if got := result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, text)
			}
			if got := searchNames(t, payload); !equalStrings(got, tc.want) {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
			if got := payload["hasMore"]; got != tc.hasMore {
				t.Errorf("hasMore = %v, want %v", got, tc.hasMore)
			}
		})
	}
}

func TestToolCatalogSearchPrefersAnExactNameMatch(t *testing.T) {
	// "alpha-extended" matches the query in its name, its description, and its
	// schema, outscoring "alpha" on every individual signal. A query whose
	// terms are exactly a tool's name terms still wins, because naming a tool
	// outright is a stronger intent than mentioning it.
	exact := server.NewTool("alpha", "Handles requests").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("a"), nil })
	broader := server.NewTool("alpha-extended", "Handles alpha requests for alpha clients").
		WithSchema(func(s *schema.Object) { s.String("alpha") }).
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("b"), nil })

	s := server.New("demo", "1.0.0", server.WithToolCatalog(broader, exact))

	_, payload, _ := callCatalogTool(t, s, "search_tools", `{"query":"alpha"}`)
	if got, want := searchNames(t, payload), []string{"alpha", "alpha-extended"}; !equalStrings(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	// The boost needs every query term to be a name term: a query that only
	// overlaps the name falls back to plain term scoring, where the broader
	// tool wins.
	_, payload, _ = callCatalogTool(t, s, "search_tools", `{"query":"alpha requests"}`)
	if got, want := searchNames(t, payload), []string{"alpha-extended", "alpha"}; !equalStrings(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

func TestToolCatalogSearchFoldsCaseOnBothSides(t *testing.T) {
	// Every searchable field of the fixture carries upper case and every query
	// is lower case, so a match proves the catalog side is folded too.
	issue := server.NewTool("Create-GitHub-Issue", "Opens a TICKET in the tracker").
		WithSchema(func(s *schema.Object) { s.String("Repository").Description("Target REPO") }).
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("i"), nil })
	plain := server.NewTool("plain-tool", "Does nothing at all").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("p"), nil })

	s := server.New("demo", "1.0.0", server.WithToolCatalog(issue, plain))

	tests := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "a lower-case query matches a mixed-case name",
			args: `{"query":"github"}`,
			want: []string{"Create-GitHub-Issue"},
		},
		{
			name: "a lower-case query matches an upper-case description word",
			args: `{"query":"ticket"}`,
			want: []string{"Create-GitHub-Issue"},
		},
		{
			name: "a lower-case query matches mixed-case schema text",
			args: `{"query":"repo"}`,
			want: []string{"Create-GitHub-Issue"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, payload, text := callCatalogTool(t, s, "search_tools", tc.args)
			if got := result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, text)
			}
			if got := searchNames(t, payload); !equalStrings(got, tc.want) {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("the exact-name boost folds case too", func(t *testing.T) {
		// Both tools carry all three query terms in their name, so they tie on
		// term scoring and registration order would put the first one first.
		// Only the boost, which needs the mixed-case name folded before it is
		// split into terms, moves the exactly named tool ahead of it.
		other := server.NewTool("issue-create-github-extended", "Does more").
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("o"), nil })
		boosted := server.New("demo", "1.0.0", server.WithToolCatalog(other, issue))

		_, payload, _ := callCatalogTool(t, boosted, "search_tools", `{"query":"create github issue"}`)
		want := []string{"Create-GitHub-Issue", "issue-create-github-extended"}
		if got := searchNames(t, payload); !equalStrings(got, want) {
			t.Errorf("tools = %v, want %v", got, want)
		}
	})
}

// markupTool returns markup in every channel a catalog reply carries it in: its
// description and schema feed a search payload, its output feeds a batch reply.
// Each "<", ">" and "&" is one character of payload text but six bytes of the
// JSON the client receives, which is the difference the budget has to count.
func markupTool() server.Tool {
	return server.NewTool("html-tool", "Wraps <input> in a & b "+strings.Repeat("<b>&</b> ", 60)).
		WithSchema(func(s *schema.Object) { s.String("markup").Description("Raw <html> & text to wrap") }).
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text(`<result a="1" & b='2'>` + strings.Repeat("<b>&</b>", 150)), nil
		})
}

func TestToolCatalogPayloadsKeepTheirBytes(t *testing.T) {
	// The payload text a meta tool returns is read by a model, so markup in it
	// is written as it stands rather than as escape sequences.
	markup := markupTool()
	s := server.New("demo", "1.0.0", server.WithToolCatalog(markup))

	tests := []struct {
		name string
		tool string
		args string
		want string
	}{
		{
			name: "search entries",
			tool: "search_tools",
			args: `{"query":"<input>"}`,
			want: `{"ok":true,"tools":[{"name":"html-tool","description":"Wraps <input> in a & b ` + strings.Repeat("<b>&</b> ", 60) + `","inputSchema":{"properties":{"markup":{"description":"Raw <html> & text to wrap","type":"string"}},"type":"object"}}],"hasMore":false}`,
		},
		{
			name: "batch results",
			tool: "execute_tools",
			args: `{"calls":[{"name":"html-tool","arguments":{}}]}`,
			want: `{"ok":true,"complete":true,"results":[{"name":"html-tool","index":0,"isError":false,"contentItems":1}],"requestedToolCalls":1,"attemptedToolCalls":1,"reportedToolCalls":1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := callCatalog(t, s, tc.tool, tc.args)
			if got := reply.result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
			}
			if reply.text != tc.want {
				t.Fatalf("payload text =\n%s\nwant\n%s", reply.text, tc.want)
			}
			// The same characters on the wire are the six-byte escapes the
			// budget has to count, whatever the payload text spells them as.
			for _, escaped := range []string{"\\u003c", "\\u003e", "\\u0026"} {
				if !strings.Contains(string(reply.wire), escaped) {
					t.Errorf("the response frame carries no %s: %s", escaped, reply.wire)
				}
			}
		})
	}
}

// TestToolCatalogBudgetCountsEscapedBytes pins the budget to the bytes the
// client receives rather than to the shorter form the payload text is written
// in. A tool returning markup, XML or code pays six bytes for each "<", ">" and
// "&", and a limit measured on the unescaped text would let such a tool run six
// times past the limit it was given.
func TestToolCatalogBudgetCountsEscapedBytes(t *testing.T) {
	markup := markupTool()

	tests := []struct {
		name string
		tool string
		args string
	}{
		{name: "search", tool: "search_tools", args: `{"query":"<input>"}`},
		{name: "execute", tool: "execute_tools", args: `{"calls":[{"name":"html-tool","arguments":{}}]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wide := server.New("demo", "1.0.0",
				server.WithToolCatalog(markup),
				server.WithToolCatalogLimits(25, 1<<20),
			)
			sent := callCatalog(t, wide, tc.tool, tc.args)
			if got := sent.result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, sent.text)
			}

			// The budget is the size of that very reply with its markup written
			// raw: everything the client receives except the escaping.
			unescaped := unescapedSize(t, sent.wire)
			if onWire := replySize(t, sent.wire); onWire <= unescaped {
				t.Fatalf("the reply is %d bytes on the wire and %d unescaped; the case needs markup that costs more escaped", onWire, unescaped)
			}

			sized := server.New("demo", "1.0.0",
				server.WithToolCatalog(markup),
				server.WithToolCatalogLimits(25, unescaped),
			)
			reply := callCatalog(t, sized, tc.tool, tc.args)
			if got := reply.result["isError"]; got != true {
				t.Fatalf("a reply of %d bytes was served under a %d byte budget: isError = %v (text %q)",
					replySize(t, sent.wire), unescaped, got, reply.text)
			}
			want := map[string]any{
				"kind":    "OutputLimitExceeded",
				"message": fmt.Sprintf("The tool output exceeded %d bytes.", unescaped),
			}
			if got := reply.payload["error"]; !jsonEqual(got, want) {
				t.Fatalf("error = %s, want %s", mustJSON(got), mustJSON(want))
			}
			assertWithinBudget(t, reply, unescaped)
		})
	}
}

// unescapedSize measures a reply the way a size check that ignores JSON string
// escaping would: the same result, re-encoded with markup written raw. It is
// always at most the size of the bytes that were really sent, so a budget taken
// from it must reject the reply it was taken from.
func unescapedSize(t *testing.T, wire json.RawMessage) int {
	t.Helper()
	var members map[string]any
	if err := json.Unmarshal(wire, &members); err != nil {
		t.Fatalf("decode result frame: %v", err)
	}
	for _, key := range []string{"resultType", "ttlMs", "cacheScope"} {
		delete(members, key)
	}
	if meta, ok := members["_meta"].(map[string]any); ok {
		delete(meta, server.MetaKeyServerInfo)
		if len(meta) == 0 {
			delete(members, "_meta")
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(members); err != nil {
		t.Fatalf("encode reply: %v", err)
	}
	return len(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func TestToolCatalogBatchSplitsAResultAcrossTheResultChannels(t *testing.T) {
	// A batch reply carries an inner result in the channels the specification
	// defines for it and not in the text a model reads: the report states what
	// ran, the content items are forwarded as content items of the reply, and
	// the metadata the tool addressed to the host travels in the reply's
	// metadata channel.
	rich := server.NewTool("rich-tool", "Returns every result field").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text("ok").
				WithMeta("trace", "t-1").
				WithStructuredContent(map[string]any{"count": 1}), nil
		})
	s := server.New("demo", "1.0.0", server.WithToolCatalog(rich))

	result, payload, text := callCatalogTool(t, s, "execute_tools", `{"calls":[{"name":"rich-tool","arguments":{}}]}`)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false (text %q)", got, text)
	}

	want := `{"ok":true,"complete":true,"results":[{"name":"rich-tool","index":0,"isError":false,"contentItems":1,"structuredContent":{"count":1}}],"requestedToolCalls":1,"attemptedToolCalls":1,"reportedToolCalls":1}`
	if text != want {
		t.Fatalf("report text =\n%s\nwant\n%s", text, want)
	}
	if strings.Contains(text, "t-1") {
		t.Errorf("the report repeats the tool metadata into model-visible text: %s", text)
	}

	wantContent := []any{map[string]any{"type": "text", "text": "ok"}}
	if got := batchResults(t, result, payload)[0]["content"]; !jsonEqual(got, wantContent) {
		t.Errorf("forwarded content = %s, want %s", mustJSON(got), mustJSON(wantContent))
	}

	meta, _ := result["_meta"].(map[string]any)
	wantMeta := map[string]any{"results": []any{map[string]any{
		"index": float64(0),
		"name":  "rich-tool",
		"meta":  map[string]any{"trace": "t-1"},
	}}}
	if got := meta["com.velocitykode.mcp/toolCatalog"]; !jsonEqual(got, wantMeta) {
		t.Errorf("_meta[com.velocitykode.mcp/toolCatalog] = %s, want %s", mustJSON(got), mustJSON(wantMeta))
	}
}

func TestToolCatalogForwardsTypedContentAndItsAnnotations(t *testing.T) {
	// Image and audio items and their audience annotations reach the client as
	// the items they are: rendering and audience filtering both depend on the
	// item's own shape, which text carrying a JSON transcript of it cannot
	// provide.
	image := content.NewImage([]byte{0x01, 0x02, 0x03}, "image/png")
	if err := image.SetAudience(content.RoleUser); err != nil {
		t.Fatalf("set audience: %v", err)
	}
	audio := content.NewAudio([]byte{0x04}, "audio/wav")

	media := server.NewTool("media-tool", "Returns an image and an audio clip").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.NewResponse(image, audio), nil
		})
	s := server.New("demo", "1.0.0", server.WithToolCatalog(media))

	result, payload, text := callCatalogTool(t, s, "execute_tools", `{"calls":[{"name":"media-tool","arguments":{}}]}`)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false (text %q)", got, text)
	}

	direct := server.New("demo", "1.0.0", server.WithTools(media))
	directResult := decodeResult(t, handle(t, direct,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"media-tool","arguments":{}}}`).Response)
	want, _ := directResult["content"].([]any)

	got := batchResults(t, result, payload)[0]["content"]
	if !jsonEqual(got, want) {
		t.Fatalf("forwarded content = %s, want the items a direct call returns %s", mustJSON(got), mustJSON(want))
	}
	items, _ := got.([]any)
	first, _ := items[0].(map[string]any)
	if kind := first["type"]; kind != "image" {
		t.Fatalf("first forwarded item type = %v, want image", kind)
	}
	annotations, _ := first["annotations"].(map[string]any)
	if audience := annotations["audience"]; !jsonEqual(audience, []any{"user"}) {
		t.Errorf("forwarded audience = %s, want [\"user\"]", mustJSON(audience))
	}
}

func TestToolCatalogSearchRejectsBadArguments(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool()))

	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "limit below the range",
			args: `{"limit":0}`,
			want: "The limit field must be between 1 and 50.",
		},
		{
			name: "limit above the range",
			args: `{"limit":51}`,
			want: "The limit field must be between 1 and 50.",
		},
		{
			name: "fractional limit",
			args: `{"limit":1.5}`,
			want: "The limit field must be an integer.",
		},
		{
			name: "limit of the wrong type",
			args: `{"limit":"ten"}`,
			want: "The limit field must be an integer.",
		},
		{
			name: "query of the wrong type",
			args: `{"query":42}`,
			want: "The query field must be a string.",
		},
		{
			name: "oversized query",
			args: `{"query":"` + strings.Repeat("x", 4097) + `"}`,
			want: "The query field must not exceed 4096 characters.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, _, text := callCatalogTool(t, s, "search_tools", tc.args)
			if got := result["isError"]; got != true {
				t.Fatalf("isError = %v, want true (text %q)", got, text)
			}
			if text != tc.want {
				t.Errorf("message = %q, want %q", text, tc.want)
			}
		})
	}
}

func TestToolCatalogSearchOutputLimit(t *testing.T) {
	t.Run("a single entry that does not fit reports the limit", func(t *testing.T) {
		// One entry on its own already outgrows the budget, so there is
		// nothing partial to return.
		bulky := server.NewTool("bulky-tool", strings.Repeat("x", 4096)).
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("bulky"), nil
			})
		s := server.New("demo", "1.0.0",
			server.WithToolCatalog(bulky),
			server.WithToolCatalogLimits(25, 1024),
		)

		reply := callCatalog(t, s, "search_tools", `{}`)
		if got := reply.result["isError"]; got != true {
			t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
		}
		want := map[string]any{
			"ok": false,
			"error": map[string]any{
				"kind":    "OutputLimitExceeded",
				"message": "The tool output exceeded 1024 bytes.",
			},
		}
		if !jsonEqual(reply.payload, want) {
			t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
		}
		assertWithinBudget(t, reply, 1024)
	})

	t.Run("entries that fit are returned and more is reported", func(t *testing.T) {
		// The budget is the size of the reply that carries the first entry
		// alone, so the second cannot join it. The description only takes that
		// reply past the smallest budget the catalog accepts.
		first := server.NewTool("first-tool", strings.Repeat("padding ", 100)).
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("ok"), nil
			})
		budget := measureReply(t, []server.Tool{first}, "search_tools", `{}`)
		s := server.New("demo", "1.0.0",
			server.WithToolCatalog(first, structuredTool()),
			server.WithToolCatalogLimits(25, budget),
		)

		reply := callCatalog(t, s, "search_tools", `{}`)
		if got := reply.result["isError"]; got != false {
			t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
		}
		if got, want := searchNames(t, reply.payload), []string{"first-tool"}; !equalStrings(got, want) {
			t.Fatalf("tools = %v, want %v", got, want)
		}
		if got := reply.payload["hasMore"]; got != true {
			t.Errorf("hasMore = %v, want true", got)
		}
		assertWithinBudget(t, reply, budget)
	})
}

func TestToolCatalogExecutesCallsInOrder(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), structuredTool()))

	result, payload, text := callCatalogTool(t, s, "execute_tools",
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}},{"name":"structured-content-tool","arguments":{}}]}`)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false (text %q)", got, text)
	}

	wantReport := map[string]any{
		"ok":       true,
		"complete": true,
		"results": []any{
			map[string]any{
				"name":         "say-hi-tool",
				"index":        float64(0),
				"isError":      false,
				"contentItems": float64(1),
			},
			map[string]any{
				"name":         "structured-content-tool",
				"index":        float64(1),
				"isError":      false,
				"contentItems": float64(1),
				"structuredContent": map[string]any{
					"conditions":  "Partly cloudy",
					"humidity":    float64(65),
					"temperature": 22.5,
				},
			},
		},
		"requestedToolCalls": float64(2),
		"attemptedToolCalls": float64(2),
		"reportedToolCalls":  float64(2),
	}
	if !jsonEqual(payload, wantReport) {
		t.Fatalf("report = %s, want %s", mustJSON(payload), mustJSON(wantReport))
	}

	// The content of each call is forwarded in call order, so the items a
	// client reads back belong to the calls the report names.
	wantContent := []any{
		map[string]any{"type": "text", "text": "Hello, Ada!"},
		map[string]any{"type": "text", "text": `{"conditions":"Partly cloudy","humidity":65,"temperature":22.5}`},
	}
	results := batchResults(t, result, payload)
	got := []any{results[0]["content"].([]any)[0], results[1]["content"].([]any)[0]}
	if !jsonEqual(got, wantContent) {
		t.Fatalf("forwarded content = %s, want %s", mustJSON(got), mustJSON(wantContent))
	}
}

func TestToolCatalogExecuteStopsOnFirstError(t *testing.T) {
	recorder := &recordingTool{}
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), recorder))

	// The first call fails its own argument validation, so the second must
	// never run.
	result, payload, text := callCatalogTool(t, s, "execute_tools",
		`{"calls":[{"name":"say-hi-tool","arguments":{}},{"name":"recording-tool","arguments":{"value":"not-run"}}]}`)

	if got := result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, text)
	}
	if got := payload["ok"]; got != false {
		t.Errorf("ok = %v, want false", got)
	}
	if got, want := resultNames(t, payload), []string{"say-hi-tool"}; !equalStrings(got, want) {
		t.Errorf("results = %v, want %v", got, want)
	}
	entries, _ := payload["results"].([]any)
	entry, _ := entries[0].(map[string]any)
	if got := entry["isError"]; got != true {
		t.Errorf("first result isError = %v, want true", got)
	}
	if got := len(recorder.calls()); got != 0 {
		t.Errorf("recording tool ran %d times, want 0", got)
	}
}

// TestToolCatalogExecuteLaunchesNoCallAfterCancellation asserts that a batch
// starts no further call once the request has been abandoned: the MCP
// cancellation notification says the receiver SHOULD stop processing the
// cancelled request, and a call that has not begun cannot observe the
// cancellation on its own, so a batch that kept going would run side effects
// for a client that is gone. The reply still carries the results of the calls
// that did run, and its counters say the rest never started.
func TestToolCatalogExecuteLaunchesNoCallAfterCancellation(t *testing.T) {
	tests := []struct {
		name        string
		abandon     bool
		wantRan     []string
		wantNames   []string
		wantContent []string
		attempted   int
		complete    bool
	}{
		{
			name:        "a request that stands runs the whole batch",
			wantRan:     []string{"second"},
			wantNames:   []string{"first-tool", "recording-tool"},
			wantContent: []string{"first", "second"},
			attempted:   2,
			complete:    true,
		},
		{
			name:        "a request abandoned mid-batch starts nothing further",
			abandon:     true,
			wantRan:     nil,
			wantNames:   []string{"first-tool"},
			wantContent: []string{"first"},
			attempted:   1,
			complete:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			recorder := &recordingTool{}
			first := server.NewTool("first-tool", "Runs while the client may go away").
				HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
					if tc.abandon {
						cancel()
					}
					return server.Text("first"), nil
				})
			s := server.New("demo", "1.0.0", server.WithToolCatalog(first, recorder))

			raw := `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"execute_tools","arguments":{"calls":[` +
				`{"name":"first-tool","arguments":{}},` +
				`{"name":"recording-tool","arguments":{"value":"second"}}]}}}`
			result := decodeResult(t, s.Handle(ctx, []byte(raw), "sess-1").Response)

			var payload map[string]any
			if err := json.Unmarshal([]byte(firstText(t, result)), &payload); err != nil {
				t.Fatalf("report is not JSON: %v", err)
			}

			if got := recorder.calls(); !equalStrings(got, tc.wantRan) {
				t.Errorf("recording tool ran with %v, want %v", got, tc.wantRan)
			}
			// Nothing failed either way: stopping for a cancelled request is
			// not an error of the calls that ran.
			if got := result["isError"]; got != false {
				t.Errorf("isError = %v, want false", got)
			}
			if got := payload["ok"]; got != true {
				t.Errorf("ok = %v, want true (payload %s)", got, mustJSON(payload))
			}
			if got := payload["complete"]; got != tc.complete {
				t.Errorf("complete = %v, want %v (payload %s)", got, tc.complete, mustJSON(payload))
			}
			counters := map[string]int{
				"requestedToolCalls": 2,
				"attemptedToolCalls": tc.attempted,
				"reportedToolCalls":  tc.attempted,
			}
			for key, want := range counters {
				if got := payload[key]; got != float64(want) {
					t.Errorf("%s = %v, want %d (payload %s)", key, got, want, mustJSON(payload))
				}
			}
			if got := resultNames(t, payload); !equalStrings(got, tc.wantNames) {
				t.Errorf("results = %v, want %v", got, tc.wantNames)
			}
			forwarded := batchContent(t, result)
			if len(forwarded) != len(tc.wantContent) {
				t.Fatalf("forwarded %d content items, want %d: %s", len(forwarded), len(tc.wantContent), mustJSON(result))
			}
			for i, want := range tc.wantContent {
				if got := forwarded[i]["text"]; got != want {
					t.Errorf("forwarded content %d = %v, want %q", i, got, want)
				}
			}
		})
	}
}

func TestToolCatalogExecuteIsolatesPerCallFailures(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), failingTool(), rpcFailingTool()))

	tests := []struct {
		name string
		call string
		want string
	}{
		{
			name: "an unknown tool is reported as a failed call",
			call: `{"name":"nope","arguments":{}}`,
			want: "Tool [nope] was not found in the catalog.",
		},
		{
			name: "an inner validation failure carries the field message",
			call: `{"name":"say-hi-tool","arguments":{}}`,
			want: "The name field is required.",
		},
		{
			name: "an internal failure never leaks its detail",
			call: `{"name":"failing-tool","arguments":{}}`,
			want: internalFailureText,
		},
		{
			name: "a protocol error keeps the message a direct call would send",
			call: `{"name":"denied-tool","arguments":{}}`,
			want: "Access to this tool is denied.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, payload, text := callCatalogTool(t, s, "execute_tools", `{"calls":[`+tc.call+`]}`)
			if got := result["isError"]; got != true {
				t.Fatalf("isError = %v, want true (text %q)", got, text)
			}
			if got := payload["ok"]; got != false {
				t.Errorf("ok = %v, want false", got)
			}
			results := batchResults(t, result, payload)
			if len(results) != 1 {
				t.Fatalf("results = %s, want one entry", mustJSON(payload["results"]))
			}
			if got := results[0]["isError"]; got != true {
				t.Errorf("result isError = %v, want true", got)
			}
			items, _ := results[0]["content"].([]any)
			item, _ := items[0].(map[string]any)
			if got := item["text"]; got != tc.want {
				t.Errorf("message = %v, want %q", got, tc.want)
			}
		})
	}

	// Both contained messages are exactly what the same failure produces on the
	// direct path: a protocol error's message is already client-facing, while
	// anything else is masked with the server's generic wording.
	directCases := []struct {
		name string
		tool server.Tool
		want string
	}{
		{name: "protocol error", tool: rpcFailingTool(), want: "Access to this tool is denied."},
		{name: "internal failure", tool: failingTool(), want: internalFailureText},
	}
	for _, tc := range directCases {
		t.Run("direct path parity: "+tc.name, func(t *testing.T) {
			direct := server.New("demo", "1.0.0", server.WithTools(tc.tool))
			res := handle(t, direct, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tc.tool.Name()))
			if res.Response == nil || res.Response.Error == nil {
				t.Fatalf("direct call should fail, got %+v", res.Response)
			}
			if got := res.Response.Error.Message; got != tc.want {
				t.Errorf("direct error message = %q, want %q", got, tc.want)
			}
		})
	}
}

// eventSummary renders a dispatched event as "<name> <tool> isError=<bool>" (or
// "<name> <tool> err=<bool>" for a failure), so a test can assert the exact
// sequence a call produced.
func eventSummary(t *testing.T, ev any) string {
	t.Helper()
	switch e := ev.(type) {
	case event.ToolCalled:
		return fmt.Sprintf("called %s isError=%t", e.Tool, e.IsError)
	case event.ToolFailed:
		return fmt.Sprintf("failed %s err=%t", e.Tool, e.Err != nil)
	default:
		t.Fatalf("unexpected event %T", ev)
		return ""
	}
}

func TestToolCatalogInnerCallsDispatchEvents(t *testing.T) {
	tests := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "one event per inner call, then the batch itself",
			args: `{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}},{"name":"structured-content-tool","arguments":{}}]}`,
			want: []string{
				"called say-hi-tool isError=false",
				"called structured-content-tool isError=false",
				"called execute_tools isError=false",
			},
		},
		{
			name: "an inner validation failure is a completed call with isError",
			args: `{"calls":[{"name":"say-hi-tool","arguments":{}}]}`,
			want: []string{
				"called say-hi-tool isError=true",
				"called execute_tools isError=true",
			},
		},
		{
			name: "an inner handler failure is reported as a failed call",
			args: `{"calls":[{"name":"failing-tool","arguments":{}}]}`,
			want: []string{
				"failed failing-tool err=true",
				"called execute_tools isError=true",
			},
		},
		{
			name: "an unknown inner tool never reaches a handler",
			args: `{"calls":[{"name":"nope","arguments":{}}]}`,
			want: []string{
				"failed nope err=false",
				"called execute_tools isError=true",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), structuredTool(), failingTool()))
			d := &recordingDispatcher{}
			s.SetEventDispatcher(d.fn())

			callCatalogTool(t, s, "execute_tools", tc.args)

			events := d.all()
			got := make([]string, 0, len(events))
			for _, ev := range events {
				got = append(got, eventSummary(t, ev))
			}
			if !equalStrings(got, tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolCatalogInnerEventsCarryTheCallContext(t *testing.T) {
	logger := &countingLogger{}
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(sayHiTool(), failingTool()),
		server.WithLogger(logger),
	)
	d := &recordingDispatcher{}
	s.SetEventDispatcher(d.fn())

	raw := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"execute_tools","arguments":{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}},{"name":"failing-tool","arguments":{"attempt":2}}]}}}`
	if res := s.Handle(context.Background(), []byte(raw), "sess-77"); res.Response == nil || res.Response.Error != nil {
		t.Fatalf("unexpected response: %+v", res.Response)
	}

	events := d.all()
	if len(events) != 3 {
		t.Fatalf("dispatched %d events, want 3: %v", len(events), events)
	}

	called, ok := events[0].(event.ToolCalled)
	if !ok {
		t.Fatalf("first event = %T, want ToolCalled", events[0])
	}
	if called.SessionID != "sess-77" {
		t.Errorf("inner ToolCalled session id = %q, want sess-77", called.SessionID)
	}
	if !jsonEqual(called.Arguments, map[string]any{"name": "Ada"}) {
		t.Errorf("inner ToolCalled arguments = %s, want {\"name\":\"Ada\"}", mustJSON(called.Arguments))
	}

	failed, ok := events[1].(event.ToolFailed)
	if !ok {
		t.Fatalf("second event = %T, want ToolFailed", events[1])
	}
	if failed.SessionID != "sess-77" {
		t.Errorf("inner ToolFailed session id = %q, want sess-77", failed.SessionID)
	}
	// A failure carries the same call context a completed call does: which
	// arguments were passed, and how long the handler ran before it failed.
	if !jsonEqual(failed.Arguments, map[string]any{"attempt": float64(2)}) {
		t.Errorf("inner ToolFailed arguments = %s, want {\"attempt\":2}", mustJSON(failed.Arguments))
	}
	if failed.Duration <= 0 {
		t.Errorf("inner ToolFailed duration = %v, want the time the handler ran", failed.Duration)
	}
	// The event keeps the underlying error for server-side diagnosis even
	// though the client only ever sees the masked message.
	if failed.Err == nil || !strings.Contains(failed.Err.Error(), "secret-token") {
		t.Errorf("inner ToolFailed err = %v, want the underlying handler error", failed.Err)
	}
	if got := logger.errors(); got != 1 {
		t.Errorf("logged %d errors, want 1 for the failed inner call", got)
	}
}

func TestToolCatalogSearchDefaultAndRequestedLimits(t *testing.T) {
	tools := make([]server.Tool, 0, 12)
	for i := range 12 {
		name := fmt.Sprintf("catalog-tool-%02d", i)
		tools = append(tools, server.NewTool(name, "A catalog entry").
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text(name), nil
			}))
	}
	s := server.New("demo", "1.0.0", server.WithToolCatalog(tools...))

	all := make([]string, 0, 12)
	for i := range 12 {
		all = append(all, fmt.Sprintf("catalog-tool-%02d", i))
	}

	tests := []struct {
		name    string
		args    string
		want    []string
		hasMore bool
	}{
		{
			name:    "an omitted limit returns the first ten",
			args:    `{}`,
			want:    all[:10],
			hasMore: true,
		},
		{
			name: "the largest accepted limit returns everything",
			args: `{"limit":50}`,
			want: all,
		},
		{
			// The boundary of the "more matched than were returned" report: as
			// many matches as the limit is not more than the limit.
			name: "a limit equal to the match count reports no more",
			args: `{"limit":12}`,
			want: all,
		},
		{
			name:    "one match past the limit reports more",
			args:    `{"limit":11}`,
			want:    all[:11],
			hasMore: true,
		},
		{
			name:    "the smallest accepted limit returns one",
			args:    `{"limit":1}`,
			want:    all[:1],
			hasMore: true,
		},
		{
			// The framework's integer rule accepts a decimal string, so a limit
			// that passed validation is honored rather than silently dropped.
			name:    "a numeric string limit is honored",
			args:    `{"limit":"5"}`,
			want:    all[:5],
			hasMore: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, payload, text := callCatalogTool(t, s, "search_tools", tc.args)
			if got := result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, text)
			}
			if got := searchNames(t, payload); !equalStrings(got, tc.want) {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
			if got := payload["hasMore"]; got != tc.hasMore {
				t.Errorf("hasMore = %v, want %v", got, tc.hasMore)
			}
		})
	}
}

func TestToolCatalogDefaultCallCap(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(echoTool()))

	batch := func(n int) string {
		calls := make([]string, 0, n)
		for i := range n {
			calls = append(calls, fmt.Sprintf(`{"name":"echo-tool","arguments":{"value":"v%d"}}`, i))
		}
		return `{"calls":[` + strings.Join(calls, ",") + `]}`
	}

	t.Run("the default cap of 25 calls is accepted", func(t *testing.T) {
		result, payload, text := callCatalogTool(t, s, "execute_tools", batch(25))
		if got := result["isError"]; got != false {
			t.Fatalf("isError = %v, want false (text %q)", got, text)
		}
		if got := payload["ok"]; got != true {
			t.Errorf("ok = %v, want true", got)
		}
		if got := len(resultNames(t, payload)); got != 25 {
			t.Errorf("results = %d, want 25", got)
		}
	})

	t.Run("one call past the default cap is rejected", func(t *testing.T) {
		result, _, text := callCatalogTool(t, s, "execute_tools", batch(26))
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true", got)
		}
		if want := "The calls field must not have more than 25 items."; text != want {
			t.Errorf("message = %q, want %q", text, want)
		}
	})

	t.Run("the default cap is advertised in the schema", func(t *testing.T) {
		listed := decodeResult(t, handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Response)
		tools, _ := listed["tools"].([]any)
		execute, _ := tools[1].(map[string]any)
		input, _ := execute["inputSchema"].(map[string]any)
		properties, _ := input["properties"].(map[string]any)
		calls, _ := properties["calls"].(map[string]any)
		if got := calls["maxItems"]; got != float64(25) {
			t.Errorf("calls maxItems = %v, want 25", got)
		}
	})
}

func TestToolCatalogDefaultOutputCap(t *testing.T) {
	// The default budget is 65536 bytes; a result well past it is reported
	// rather than truncated, and the message names the effective budget.
	bulky := server.NewTool("bulky-tool", "Returns more than the default budget").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text(strings.Repeat("y", 70000)), nil
		})
	s := server.New("demo", "1.0.0", server.WithToolCatalog(bulky))

	reply := callCatalog(t, s, "execute_tools", `{"calls":[{"name":"bulky-tool","arguments":{}}]}`)
	if got := reply.result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}
	want := map[string]any{
		"ok":       false,
		"complete": false,
		"error": map[string]any{
			"kind":    "OutputLimitExceeded",
			"message": "The tool output exceeded 65536 bytes.",
		},
		"results":            []any{},
		"omittedToolCall":    map[string]any{"name": "bulky-tool", "index": float64(0)},
		"requestedToolCalls": float64(1),
		"attemptedToolCalls": float64(1),
		"reportedToolCalls":  float64(0),
	}
	if !jsonEqual(reply.payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
	}
	assertWithinBudget(t, reply, 65536)
}

func TestToolCatalogOutputLimitBoundary(t *testing.T) {
	// Each budget is the size of a reply the catalog really sent, measured off
	// the wire under a budget far wider than the reply needed. A budget of
	// exactly that many bytes must carry the same reply; one byte less must
	// not, and what comes back instead must itself fit.
	t.Run("search", func(t *testing.T) {
		description := "Returns nothing at all. " + strings.Repeat("padding ", 150)
		tool := server.NewTool("boundary-tool", description).
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("ok"), nil
			})
		budget := measureReply(t, []server.Tool{tool}, "search_tools", `{}`)

		t.Run("exactly the budget fits", func(t *testing.T) {
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(tool),
				server.WithToolCatalogLimits(25, budget),
			)
			reply := callCatalog(t, s, "search_tools", `{}`)
			if got := reply.result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
			}
			if got, want := searchNames(t, reply.payload), []string{"boundary-tool"}; !equalStrings(got, want) {
				t.Errorf("tools = %v, want %v", got, want)
			}
			if got := reply.payload["hasMore"]; got != false {
				t.Errorf("hasMore = %v, want false", got)
			}
			if got := replySize(t, reply.wire); got != budget {
				t.Errorf("the reply is %d bytes, want the %d it measured", got, budget)
			}
		})

		t.Run("one byte less does not", func(t *testing.T) {
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(tool),
				server.WithToolCatalogLimits(25, budget-1),
			)
			reply := callCatalog(t, s, "search_tools", `{}`)
			if got := reply.result["isError"]; got != true {
				t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
			}
			want := map[string]any{
				"ok": false,
				"error": map[string]any{
					"kind":    "OutputLimitExceeded",
					"message": fmt.Sprintf("The tool output exceeded %d bytes.", budget-1),
				},
			}
			if !jsonEqual(reply.payload, want) {
				t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
			}
			assertWithinBudget(t, reply, budget-1)
		})
	})

	t.Run("execute", func(t *testing.T) {
		value := strings.Repeat("z", 1200)
		arguments := fmt.Sprintf(`{"calls":[{"name":"echo-tool","arguments":{"value":%q}}]}`, value)
		budget := measureReply(t, []server.Tool{echoTool()}, "execute_tools", arguments)

		t.Run("exactly the budget fits", func(t *testing.T) {
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(echoTool()),
				server.WithToolCatalogLimits(25, budget),
			)
			reply := callCatalog(t, s, "execute_tools", arguments)
			if got := reply.result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
			}
			if got, want := resultNames(t, reply.payload), []string{"echo-tool"}; !equalStrings(got, want) {
				t.Errorf("results = %v, want %v", got, want)
			}
			if got := replySize(t, reply.wire); got != budget {
				t.Errorf("the reply is %d bytes, want the %d it measured", got, budget)
			}
		})

		t.Run("one byte less does not", func(t *testing.T) {
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(echoTool()),
				server.WithToolCatalogLimits(25, budget-1),
			)
			reply := callCatalog(t, s, "execute_tools", arguments)
			if got := reply.result["isError"]; got != true {
				t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
			}
			// Nothing ran before the oversized call, so the report carries no
			// results: it names the one call whose output did not fit.
			want := map[string]any{
				"ok":       false,
				"complete": false,
				"error": map[string]any{
					"kind":    "OutputLimitExceeded",
					"message": fmt.Sprintf("The tool output exceeded %d bytes.", budget-1),
				},
				"results":            []any{},
				"omittedToolCall":    map[string]any{"name": "echo-tool", "index": float64(0)},
				"requestedToolCalls": float64(1),
				"attemptedToolCalls": float64(1),
				"reportedToolCalls":  float64(0),
			}
			if !jsonEqual(reply.payload, want) {
				t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
			}
			assertWithinBudget(t, reply, budget-1)
		})
	})
}

func TestToolCatalogExecuteKeepsTheResultsOfCallsThatAlreadyRan(t *testing.T) {
	value := strings.Repeat("z", 1200)
	call := fmt.Sprintf(`{"name":"echo-tool","arguments":{"value":%q}}`, value)
	// A reply carrying one of these calls plus the diagnostics the second one
	// forces: measured off the wire from a batch of one, widened by the size a
	// failure report adds. The batch of two then crosses it on the second call
	// while the first result still has room.
	one := measureReply(t, []server.Tool{echoTool()}, "execute_tools", `{"calls":[`+call+`]}`)
	budget := one + 512

	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(echoTool()),
		server.WithToolCatalogLimits(25, budget),
	)
	reply := callCatalog(t, s, "execute_tools", `{"calls":[`+call+`,`+call+`]}`)
	result, payload := reply.result, reply.payload

	if got := result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}
	// Both calls ran. The first one's result is reported whole, because a call
	// that already changed something must never have to be run again to
	// recover what it returned; the second is named as the one whose output
	// did not fit.
	want := map[string]any{
		"ok":       false,
		"complete": false,
		"error": map[string]any{
			"kind":    "OutputLimitExceeded",
			"message": fmt.Sprintf("The tool output exceeded %d bytes.", budget),
		},
		"results": []any{map[string]any{
			"name":         "echo-tool",
			"index":        float64(0),
			"isError":      false,
			"contentItems": float64(1),
		}},
		"omittedToolCall":    map[string]any{"name": "echo-tool", "index": float64(1)},
		"requestedToolCalls": float64(2),
		"attemptedToolCalls": float64(2),
		"reportedToolCalls":  float64(1),
	}
	if !jsonEqual(payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(payload), mustJSON(want))
	}
	// What the first call returned is still there to read.
	results := batchResults(t, result, payload)
	wantContent := []any{map[string]any{"type": "text", "text": value}}
	if got := results[0]["content"]; !jsonEqual(got, wantContent) {
		t.Fatalf("kept content = %s, want the first call's own result", mustJSON(got))
	}
	// The reply is held to the limit it reports.
	assertWithinBudget(t, reply, budget)
}

// TestToolCatalogExecuteRecoversAMutationWhoseBatchOverflowed is the reason the
// completed prefix is kept: the identifier a mutating call returns is the only
// handle a client has on what it created, and running the batch again to
// recover it would create a second one.
func TestToolCatalogExecuteRecoversAMutationWhoseBatchOverflowed(t *testing.T) {
	var orders atomic.Int64
	create := server.NewTool("create-order", "Creates an order and returns its id").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text(fmt.Sprintf("order-%d", orders.Add(1))), nil
		})
	bulky := server.NewTool("read-catalog", "Reads far more than the budget carries").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text(strings.Repeat("y", 4096)), nil
		})
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(create, bulky),
		server.WithToolCatalogLimits(25, 1024),
	)

	reply := callCatalog(t, s, "execute_tools",
		`{"calls":[{"name":"create-order","arguments":{}},{"name":"read-catalog","arguments":{}}]}`)
	result, payload := reply.result, reply.payload
	if got := result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}

	want := map[string]any{
		"ok":       false,
		"complete": false,
		"error": map[string]any{
			"kind":    "OutputLimitExceeded",
			"message": "The tool output exceeded 1024 bytes.",
		},
		"results": []any{map[string]any{
			"name":         "create-order",
			"index":        float64(0),
			"isError":      false,
			"contentItems": float64(1),
		}},
		"omittedToolCall":    map[string]any{"name": "read-catalog", "index": float64(1)},
		"requestedToolCalls": float64(2),
		"attemptedToolCalls": float64(2),
		"reportedToolCalls":  float64(1),
	}
	if !jsonEqual(payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(payload), mustJSON(want))
	}
	// The identifier the mutation returned is still readable.
	wantContent := []any{map[string]any{"type": "text", "text": "order-1"}}
	if got := batchResults(t, result, payload)[0]["content"]; !jsonEqual(got, wantContent) {
		t.Fatalf("kept content = %s, want %s", mustJSON(got), mustJSON(wantContent))
	}
	if got := orders.Load(); got != 1 {
		t.Fatalf("the mutating tool ran %d times, want 1", got)
	}
	assertWithinBudget(t, reply, 1024)
}

// TestToolCatalogExecuteStopsRatherThanLoseAResult pins the rule at the only
// budget where it bites: one that carries a completed result but not that
// result beside the diagnostics of a later call. The batch does not run the
// later call at all, because a result that was produced is worth more than the
// diagnostics of one that was never needed, and the counters say where it
// stopped so the client can reissue the rest.
func TestToolCatalogExecuteStopsRatherThanLoseAResult(t *testing.T) {
	value := strings.Repeat("z", 1200)
	call := fmt.Sprintf(`{"name":"echo-tool","arguments":{"value":%q}}`, value)
	// The budget is the reply a batch of one produced plus a margin that sits
	// between two known costs: reporting the same result in a batch of two
	// costs one byte more (the completeness flag reads false rather than true,
	// and the requested count stays one digit), while the diagnostics of a
	// second call cost hundreds. So the first result fits and no report of a
	// second call's failure can, which is the window this test needs. The
	// assertions below prove the reply really did fit and the second call
	// really was never made.
	budget := measureReply(t, []server.Tool{echoTool()}, "execute_tools", `{"calls":[`+call+`]}`) + 16

	tests := []struct {
		name   string
		second func(ran *atomic.Int64) server.Tool
		call   string
	}{
		{
			name: "a call whose result would not fit",
			second: func(ran *atomic.Int64) server.Tool {
				return server.NewTool("bulky-tool", "Returns far more than the budget carries").
					HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
						ran.Add(1)
						return server.Text(strings.Repeat("y", 4096)), nil
					})
			},
			call: `{"name":"bulky-tool","arguments":{}}`,
		},
		{
			name: "a call whose result cannot be encoded",
			second: func(ran *atomic.Int64) server.Tool {
				return server.NewTool("unencodable-tool", "Returns structured content that cannot be encoded").
					HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
						ran.Add(1)
						return server.Text("ok").WithStructuredContent(map[string]any{"ch": make(chan int)}), nil
					})
			},
			call: `{"name":"unencodable-tool","arguments":{}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Int64
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(echoTool(), tc.second(&ran)),
				server.WithToolCatalogLimits(25, budget),
			)
			reply := callCatalog(t, s, "execute_tools", `{"calls":[`+call+`,`+tc.call+`]}`)

			if got := reply.result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
			}
			assertWithinBudget(t, reply, budget)
			// The first call ran and its result is reported whole; the second
			// never started, which the counters state by leaving one call
			// unattempted.
			want := map[string]any{
				"ok":       true,
				"complete": false,
				"results": []any{map[string]any{
					"name":         "echo-tool",
					"index":        float64(0),
					"isError":      false,
					"contentItems": float64(1),
				}},
				"requestedToolCalls": float64(2),
				"attemptedToolCalls": float64(1),
				"reportedToolCalls":  float64(1),
			}
			if !jsonEqual(reply.payload, want) {
				t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
			}
			if got := ran.Load(); got != 0 {
				t.Fatalf("the second tool ran %d times: a call whose outcome could not be reported must not run", got)
			}
			// What the call that did run returned is still there to read.
			wantContent := []any{map[string]any{"type": "text", "text": value}}
			if got := batchResults(t, reply.result, reply.payload)[0]["content"]; !jsonEqual(got, wantContent) {
				t.Fatalf("kept content = %s, want the first call's own result", mustJSON(got))
			}
		})
	}
}

// TestToolCatalogExecuteReportsWhetherTheBatchIsComplete pins the two flags an
// execute reply leads with, across every outcome the batch has. "ok" answers
// whether the calls the report carries a record for all succeeded; "complete"
// answers whether the batch covered the request, meaning every call was
// attempted and every attempted call has a record. The two are independent, and
// a host that reads them without comparing the three counters must still never
// read a batch that stopped early as a finished one: a batch that stopped for
// room succeeded at everything it ran, so it reports ok, and only the
// completeness flag keeps that from meaning the whole request was carried out.
func TestToolCatalogExecuteReportsWhetherTheBatchIsComplete(t *testing.T) {
	bulky := server.NewTool("bulky-tool", "Returns far more than the budget carries").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text(strings.Repeat("y", 4096)), nil
		})
	value := strings.Repeat("z", 1200)
	sized := fmt.Sprintf(`{"name":"echo-tool","arguments":{"value":%q}}`, value)
	one := measureReply(t, []server.Tool{echoTool()}, "execute_tools", `{"calls":[`+sized+`]}`)

	tests := []struct {
		name      string
		tools     []server.Tool
		budget    int
		calls     string
		ok        bool
		complete  bool
		requested int
		attempted int
		reported  int
	}{
		{
			name:   "every requested call ran and is reported",
			tools:  []server.Tool{echoTool()},
			budget: 1 << 20,
			calls: `{"calls":[{"name":"echo-tool","arguments":{"value":"a"}},` +
				`{"name":"echo-tool","arguments":{"value":"b"}}]}`,
			ok: true, complete: true,
			requested: 2, attempted: 2, reported: 2,
		},
		{
			name:   "a failing call leaves the rest of the batch unrun",
			tools:  []server.Tool{echoTool(), failingTool()},
			budget: 1 << 20,
			calls: `{"calls":[{"name":"failing-tool","arguments":{}},` +
				`{"name":"echo-tool","arguments":{"value":"never"}}]}`,
			ok: false, complete: false,
			requested: 2, attempted: 1, reported: 1,
		},
		{
			name:   "a batch whose last call fails still covered the request",
			tools:  []server.Tool{echoTool(), failingTool()},
			budget: 1 << 20,
			calls: `{"calls":[{"name":"echo-tool","arguments":{"value":"a"}},` +
				`{"name":"failing-tool","arguments":{}}]}`,
			ok: false, complete: true,
			requested: 2, attempted: 2, reported: 2,
		},
		{
			// The margin sits between the one byte a batch of two adds to the
			// same result and the hundreds a second call's diagnostics cost,
			// so the first result fits and no report of a second call can.
			name:   "a batch that stops for room succeeded without finishing",
			tools:  []server.Tool{echoTool(), bulky},
			budget: one + 16,
			calls:  `{"calls":[` + sized + `,{"name":"bulky-tool","arguments":{}}]}`,
			ok:     true, complete: false,
			requested: 2, attempted: 1, reported: 1,
		},
		{
			// Widened by more than a failure report costs, so the second call
			// runs and it is its own result that cannot be carried.
			name:   "a result that could not be carried leaves the batch incomplete",
			tools:  []server.Tool{echoTool()},
			budget: one + 512,
			calls:  `{"calls":[` + sized + `,` + sized + `]}`,
			ok:     false, complete: false,
			requested: 2, attempted: 2, reported: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(tc.tools...),
				server.WithToolCatalogLimits(25, tc.budget),
			)
			reply := callCatalog(t, s, "execute_tools", tc.calls)
			assertWithinBudget(t, reply, tc.budget)

			if got := reply.payload["ok"]; got != tc.ok {
				t.Errorf("ok = %v, want %v (payload %s)", got, tc.ok, mustJSON(reply.payload))
			}
			if got := reply.payload["complete"]; got != tc.complete {
				t.Errorf("complete = %v, want %v (payload %s)", got, tc.complete, mustJSON(reply.payload))
			}
			counters := map[string]int{
				"requestedToolCalls": tc.requested,
				"attemptedToolCalls": tc.attempted,
				"reportedToolCalls":  tc.reported,
			}
			for key, want := range counters {
				if got := reply.payload[key]; got != float64(want) {
					t.Errorf("%s = %v, want %d (payload %s)", key, got, want, mustJSON(reply.payload))
				}
			}
			// The flag never disagrees with the counters it summarizes: a host
			// may read either, and both say the same thing.
			if tc.complete != (tc.requested == tc.attempted && tc.reported == tc.attempted) {
				t.Errorf("the expected flag and counters of this case disagree: %+v", tc)
			}
		})
	}
}

func TestToolCatalogOutputLimitOutranksTheErrorStop(t *testing.T) {
	// A failing call whose own output is oversized reports the budget, not the
	// results payload: the size check runs before the stop-on-error check.
	shouty := server.NewTool("shouty-tool", "Fails loudly").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Error(strings.Repeat("!", 2000)), nil
		})
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(shouty),
		server.WithToolCatalogLimits(25, 1024),
	)

	reply := callCatalog(t, s, "execute_tools", `{"calls":[{"name":"shouty-tool","arguments":{}}]}`)
	if got := reply.result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}
	want := map[string]any{
		"ok":       false,
		"complete": false,
		"error": map[string]any{
			"kind":    "OutputLimitExceeded",
			"message": "The tool output exceeded 1024 bytes.",
		},
		"results":            []any{},
		"omittedToolCall":    map[string]any{"name": "shouty-tool", "index": float64(0)},
		"requestedToolCalls": float64(1),
		"attemptedToolCalls": float64(1),
		"reportedToolCalls":  float64(0),
	}
	if !jsonEqual(reply.payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
	}
	assertWithinBudget(t, reply, 1024)
}

func TestToolCatalogSearchTermsAreUnicodeAware(t *testing.T) {
	cafe := server.NewTool("café-tool", "Serves café au lait").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("c"), nil })
	tea := server.NewTool("茶-tool", "Serves 茶").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("t"), nil })
	plain := server.NewTool("plain-tool", "Serves nothing").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) { return server.Text("p"), nil })

	s := server.New("demo", "1.0.0", server.WithToolCatalog(cafe, tea, plain))

	tests := []struct {
		name string
		args string
		want []string
	}{
		{
			name: "case folding is not limited to ASCII",
			args: `{"query":"CAFÉ"}`,
			want: []string{"café-tool"},
		},
		{
			name: "a term of non-Latin letters is a term, not a blank query",
			args: `{"query":"茶"}`,
			want: []string{"茶-tool"},
		},
		{
			name: "a control character separates terms",
			args: "{\"query\":\"café\\u0000茶\"}",
			want: []string{"café-tool", "茶-tool"},
		},
		{
			name: "a combining mark separates terms, so decomposed text does not match composed text",
			args: `{"query":"café"}`,
			want: []string{},
		},
		{
			name: "a query with no letters or digits browses the catalog",
			args: `{"query":"  --  "}`,
			want: []string{"café-tool", "茶-tool", "plain-tool"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, payload, text := callCatalogTool(t, s, "search_tools", tc.args)
			if got := result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, text)
			}
			if got := searchNames(t, payload); !equalStrings(got, tc.want) {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolCatalogLengthLimitsCountCharacters(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool()))

	t.Run("a multibyte query at the advertised maxLength is accepted", func(t *testing.T) {
		query := strings.Repeat("あ", 4096)
		if utf8.RuneCountInString(query) != 4096 || len(query) <= 4096 {
			t.Fatalf("fixture is not multibyte: %d runes, %d bytes", utf8.RuneCountInString(query), len(query))
		}
		result, payload, text := callCatalogTool(t, s, "search_tools", fmt.Sprintf(`{"query":%q}`, query))
		if got := result["isError"]; got != false {
			t.Fatalf("isError = %v, want false (text %q)", got, text)
		}
		if got := searchNames(t, payload); len(got) != 0 {
			t.Errorf("tools = %v, want none", got)
		}
	})

	t.Run("one character past the advertised maxLength is rejected", func(t *testing.T) {
		result, _, text := callCatalogTool(t, s, "search_tools", fmt.Sprintf(`{"query":%q}`, strings.Repeat("あ", 4097)))
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true", got)
		}
		if want := "The query field must not exceed 4096 characters."; text != want {
			t.Errorf("message = %q, want %q", text, want)
		}
	})

	t.Run("a multibyte call name at the advertised maxLength reaches the catalog", func(t *testing.T) {
		name := strings.Repeat("あ", 255)
		result, payload, _ := callCatalogTool(t, s, "execute_tools",
			fmt.Sprintf(`{"calls":[{"name":%q,"arguments":{}}]}`, name))
		// It passed argument validation, so it failed on lookup instead.
		if got, want := resultNames(t, payload), []string{name}; !equalStrings(got, want) {
			t.Fatalf("results = %v, want the call to have been attempted", got)
		}
		items, _ := batchResults(t, result, payload)[0]["content"].([]any)
		item, _ := items[0].(map[string]any)
		if got, want := item["text"], "Tool ["+name+"] was not found in the catalog."; got != want {
			t.Errorf("message = %v, want %q", got, want)
		}
	})

	t.Run("one character past the call name maxLength is rejected", func(t *testing.T) {
		result, _, text := callCatalogTool(t, s, "execute_tools",
			fmt.Sprintf(`{"calls":[{"name":%q,"arguments":{}}]}`, strings.Repeat("あ", 256)))
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true", got)
		}
		if want := "The calls.0.name field must not exceed 255 characters."; text != want {
			t.Errorf("message = %q, want %q", text, want)
		}
	})
}

func TestToolCatalogExecuteAcceptsNullArguments(t *testing.T) {
	// A null "arguments" is treated as no arguments, exactly as an omitted
	// "arguments" object is on a direct tools/call.
	s := server.New("demo", "1.0.0", server.WithToolCatalog(structuredTool()))

	result, payload, text := callCatalogTool(t, s, "execute_tools",
		`{"calls":[{"name":"structured-content-tool","arguments":null}]}`)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false (text %q)", got, text)
	}
	if got, want := resultNames(t, payload), []string{"structured-content-tool"}; !equalStrings(got, want) {
		t.Errorf("results = %v, want %v", got, want)
	}
}

func TestToolCatalogMetaToolSchemas(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool()))

	listed := decodeResult(t, handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Response)
	tools, _ := listed["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools/list returned %d tools, want 2", len(tools))
	}
	search, _ := tools[0].(map[string]any)
	execute, _ := tools[1].(map[string]any)

	wantSearch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"maxLength":   float64(4096),
				"description": "Search terms. An empty query browses the catalog.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"minimum":     float64(1),
				"maximum":     float64(50),
				"description": "Maximum results to return. Defaults to 10.",
			},
		},
	}
	if !jsonEqual(search["inputSchema"], wantSearch) {
		t.Errorf("search_tools inputSchema = %s, want %s", mustJSON(search["inputSchema"]), mustJSON(wantSearch))
	}

	wantExecute := map[string]any{
		"type":     "object",
		"required": []any{"calls"},
		"properties": map[string]any{
			"calls": map[string]any{
				"type":        "array",
				"description": "Independent tool calls to execute synchronously in order.",
				"minItems":    float64(1),
				"maxItems":    float64(25),
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name"},
					"properties": map[string]any{
						"name": map[string]any{
							"type":        "string",
							"maxLength":   float64(255),
							"description": "The exact tool name returned by search_tools.",
						},
						"arguments": map[string]any{
							"type":        "object",
							"description": "Arguments matching the tool input schema.",
						},
					},
				},
			},
		},
	}
	if !jsonEqual(execute["inputSchema"], wantExecute) {
		t.Errorf("execute_tools inputSchema = %s, want %s", mustJSON(execute["inputSchema"]), mustJSON(wantExecute))
	}
}

func TestToolCatalogMetaToolsNeedAnEntry(t *testing.T) {
	tests := []struct {
		name    string
		options []server.Option
		want    []string
	}{
		{
			name:    "limits alone advertise nothing",
			options: []server.Option{server.WithToolCatalogLimits(5, 1024)},
			want:    []string{},
		},
		{
			name:    "an empty catalog option advertises nothing",
			options: []server.Option{server.WithToolCatalog()},
			want:    []string{},
		},
		{
			name:    "a catalog of nil tools advertises nothing",
			options: []server.Option{server.WithToolCatalog(nil, nil)},
			want:    []string{},
		},
		{
			name: "the meta tools appear where the first entry was registered",
			options: []server.Option{
				server.WithTools(addTool()),
				server.WithToolCatalogLimits(5, 1024),
				server.WithToolCatalog(sayHiTool()),
			},
			want: []string{"add", "search_tools", "execute_tools"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", tc.options...)
			if got := toolNames(t, s); !equalStrings(got, tc.want) {
				t.Fatalf("tools/list names = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("limits set before the entries still apply", func(t *testing.T) {
		s := server.New("demo", "1.0.0",
			server.WithToolCatalogLimits(2, 1024),
			server.WithToolCatalog(echoTool()),
		)
		call := `{"name":"echo-tool","arguments":{"value":"v"}}`
		result, _, text := callCatalogTool(t, s, "execute_tools", `{"calls":[`+call+`,`+call+`,`+call+`]}`)
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true", got)
		}
		if want := "The calls field must not have more than 2 items."; text != want {
			t.Errorf("message = %q, want %q", text, want)
		}
	})
}

func TestToolCatalogContainsUnrepresentableOutput(t *testing.T) {
	t.Run("content that has no tool shape", func(t *testing.T) {
		// Blob content is valid in a resource read but not in a tool result.
		blobTool := server.NewTool("blob-tool", "Returns a blob").
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.NewResponse(content.NewBlob([]byte("raw"))), nil
			})
		s := server.New("demo", "1.0.0", server.WithToolCatalog(blobTool))

		result, payload, _ := callCatalogTool(t, s, "execute_tools", `{"calls":[{"name":"blob-tool","arguments":{}}]}`)
		results := batchResults(t, result, payload)
		if len(results) != 1 {
			t.Fatalf("results = %s, want one entry", mustJSON(payload["results"]))
		}
		if got := results[0]["isError"]; got != true {
			t.Errorf("isError = %v, want true", got)
		}
		items, _ := results[0]["content"].([]any)
		item, _ := items[0].(map[string]any)
		want := "The tool returned content that cannot be represented in a tool result."
		if got := item["text"]; got != want {
			t.Errorf("message = %v, want %q", got, want)
		}
	})

	t.Run("a schema that cannot be encoded", func(t *testing.T) {
		// The tool is still runnable; only the description of it is broken, so
		// the search reports the failure while the batch still executes.
		unencodable := server.NewTool("unencodable-schema-tool", "Declares a schema that cannot be encoded").
			WithSchema(func(s *schema.Object) { s.String("value").Default(make(chan int)) }).
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("ran"), nil
			})
		s := server.New("demo", "1.0.0", server.WithToolCatalog(unencodable))

		result, _, text := callCatalogTool(t, s, "search_tools", `{}`)
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true", got)
		}
		if want := "The tool catalog could not encode the tool output."; text != want {
			t.Errorf("message = %q, want %q", text, want)
		}

		result, payload, text := callCatalogTool(t, s, "execute_tools",
			`{"calls":[{"name":"unencodable-schema-tool","arguments":{}}]}`)
		if got := result["isError"]; got != false {
			t.Fatalf("execute isError = %v, want false (text %q)", got, text)
		}
		if got, want := resultNames(t, payload), []string{"unencodable-schema-tool"}; !equalStrings(got, want) {
			t.Errorf("results = %v, want %v", got, want)
		}
	})

	t.Run("structured content that cannot be encoded", func(t *testing.T) {
		unencodable := server.NewTool("unencodable-tool", "Returns structured content that cannot be encoded").
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("ok").WithStructuredContent(map[string]any{"ch": make(chan int)}), nil
			})
		s := server.New("demo", "1.0.0", server.WithToolCatalog(unencodable))

		result, payload, text := callCatalogTool(t, s, "execute_tools", `{"calls":[{"name":"unencodable-tool","arguments":{}}]}`)
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true (text %q)", got, text)
		}
		// The call ran, so the report says so and names it, exactly as the
		// output-limit report does: only the rendering of its result failed.
		want := map[string]any{
			"ok":       false,
			"complete": false,
			"error": map[string]any{
				"kind":    "OutputEncodingFailed",
				"message": "The tool catalog could not encode the tool output.",
			},
			"results":            []any{},
			"omittedToolCall":    map[string]any{"name": "unencodable-tool", "index": float64(0)},
			"requestedToolCalls": float64(1),
			"attemptedToolCalls": float64(1),
			"reportedToolCalls":  float64(0),
		}
		if !jsonEqual(payload, want) {
			t.Fatalf("payload = %s, want %s", mustJSON(payload), mustJSON(want))
		}
	})
}

// TestToolCatalogExecuteKeepsResultsWhenAlaterCallCannotBeEncoded is the
// encoding counterpart of the output-limit recovery: the calls before the one
// whose result cannot be rendered already ran, and a client that cannot see
// what they returned has no way to tell which of its writes happened. Running
// the batch again to find out would repeat them.
func TestToolCatalogExecuteKeepsResultsWhenAlaterCallCannotBeEncoded(t *testing.T) {
	var orders atomic.Int64
	create := server.NewTool("create-order", "Creates an order and returns its id").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text(fmt.Sprintf("order-%d", orders.Add(1))), nil
		})
	unencodable := server.NewTool("unencodable-tool", "Returns structured content that cannot be encoded").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text("ok").WithStructuredContent(map[string]any{"ch": make(chan int)}), nil
		})
	s := server.New("demo", "1.0.0", server.WithToolCatalog(create, unencodable))

	result, payload, text := callCatalogTool(t, s, "execute_tools",
		`{"calls":[{"name":"create-order","arguments":{}},{"name":"unencodable-tool","arguments":{}}]}`)
	if got := result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, text)
	}

	want := map[string]any{
		"ok":       false,
		"complete": false,
		"error": map[string]any{
			"kind":    "OutputEncodingFailed",
			"message": "The tool catalog could not encode the tool output.",
		},
		"results": []any{map[string]any{
			"name":         "create-order",
			"index":        float64(0),
			"isError":      false,
			"contentItems": float64(1),
		}},
		"omittedToolCall":    map[string]any{"name": "unencodable-tool", "index": float64(1)},
		"requestedToolCalls": float64(2),
		"attemptedToolCalls": float64(2),
		"reportedToolCalls":  float64(1),
	}
	if !jsonEqual(payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(payload), mustJSON(want))
	}
	wantContent := []any{map[string]any{"type": "text", "text": "order-1"}}
	if got := batchResults(t, result, payload)[0]["content"]; !jsonEqual(got, wantContent) {
		t.Fatalf("kept content = %s, want %s", mustJSON(got), mustJSON(wantContent))
	}
	if got := orders.Load(); got != 1 {
		t.Fatalf("the mutating tool ran %d times, want 1", got)
	}
}

// TestToolCatalogEncodeFailureReportStaysWithinTheBudget asserts the encoding
// report is held to the output limit like the limit report is, and that holding
// it there never costs a result that was already produced: under a budget that
// carries the diagnostics beside the completed result, both are reported.
func TestToolCatalogEncodeFailureReportStaysWithinTheBudget(t *testing.T) {
	value := strings.Repeat("z", 1200)
	unencodable := server.NewTool("unencodable-tool", "Returns structured content that cannot be encoded").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text("ok").WithStructuredContent(map[string]any{"ch": make(chan int)}), nil
		})

	call := fmt.Sprintf(`{"name":"echo-tool","arguments":{"value":%q}}`, value)
	// The budget is the reply the first call produced on its own, widened by
	// the room the diagnostics of a second call need beside it.
	budget := measureReply(t, []server.Tool{echoTool()}, "execute_tools", `{"calls":[`+call+`]}`) + 512
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(echoTool(), unencodable),
		server.WithToolCatalogLimits(25, budget),
	)

	reply := callCatalog(t, s, "execute_tools",
		`{"calls":[`+call+`,{"name":"unencodable-tool","arguments":{}}]}`)
	if got := reply.result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}
	assertWithinBudget(t, reply, budget)
	want := map[string]any{
		"ok":       false,
		"complete": false,
		"error": map[string]any{
			"kind":    "OutputEncodingFailed",
			"message": "The tool catalog could not encode the tool output.",
		},
		"results": []any{map[string]any{
			"name":         "echo-tool",
			"index":        float64(0),
			"isError":      false,
			"contentItems": float64(1),
		}},
		"omittedToolCall":    map[string]any{"name": "unencodable-tool", "index": float64(1)},
		"requestedToolCalls": float64(2),
		"attemptedToolCalls": float64(2),
		"reportedToolCalls":  float64(1),
	}
	if !jsonEqual(reply.payload, want) {
		t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
	}
	wantContent := []any{map[string]any{"type": "text", "text": value}}
	if got := batchResults(t, reply.result, reply.payload)[0]["content"]; !jsonEqual(got, wantContent) {
		t.Fatalf("kept content = %s, want the first call's own result", mustJSON(got))
	}
}

// TestToolCatalogClipsAHostileNameBeforeLosingAResult pins the order the limit
// report gives way in. A completed call's result and the client-supplied name
// of a later call compete for the same bytes: the name is what is clipped,
// because the client already knows it and the report's index identifies the
// call anyway, while the identifier the completed call returned exists nowhere
// else.
func TestToolCatalogClipsAHostileNameBeforeLosingAResult(t *testing.T) {
	var orders atomic.Int64
	create := server.NewTool("create-order", "Creates an order and returns its id").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text(fmt.Sprintf("order-%d", orders.Add(1))), nil
		})
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(create),
		server.WithToolCatalogLimits(25, 1024),
	)

	// The second call names no tool and spends the whole name allowance on
	// characters that cost six bytes each once serialized, so its diagnostics
	// cannot be reported in full beside the first call's result.
	hostile := strings.Repeat("\u0007", maxCatalogCallNameCharsForTest)
	args := fmt.Sprintf(`{"calls":[{"name":"create-order","arguments":{}},{"name":%s,"arguments":{}}]}`, mustJSON(hostile))
	reply := callCatalog(t, s, "execute_tools", args)

	if got := reply.result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}
	assertWithinBudget(t, reply, 1024)

	if got := orders.Load(); got != 1 {
		t.Fatalf("the mutating tool ran %d times, want 1", got)
	}
	records, _ := reply.payload["results"].([]any)
	if len(records) != 1 {
		t.Fatalf("the report carries %d records, want the one the mutation produced: %s", len(records), mustJSON(reply.payload))
	}
	wantContent := []any{map[string]any{"type": "text", "text": "order-1"}}
	if got := batchResults(t, reply.result, reply.payload)[0]["content"]; !jsonEqual(got, wantContent) {
		t.Fatalf("kept content = %s, want %s", mustJSON(got), mustJSON(wantContent))
	}

	omitted, ok := reply.payload["omittedToolCall"].(map[string]any)
	if !ok {
		t.Fatalf("payload names no omitted call: %s", mustJSON(reply.payload))
	}
	if got := omitted["index"]; got != float64(1) {
		t.Fatalf("index = %v, want 1", got)
	}
	reported, _ := omitted["name"].(string)
	if reported == hostile {
		t.Fatalf("the complete %d-character name was repeated into a 1024 byte report", len([]rune(hostile)))
	}
	if !strings.HasSuffix(reported, "...") {
		t.Fatalf("clipped name = %q, want it marked as clipped", reported)
	}
	if prefix := strings.TrimSuffix(reported, "..."); !strings.HasPrefix(hostile, prefix) {
		t.Fatalf("clipped name = %q, want a prefix of the name that was sent", reported)
	}
}

// TestToolCatalogLimitReportBoundsAHostileToolName asserts the output limit is
// honoured on input chosen to break it: a call whose name is as long as the
// schema permits, addressing no tool at all, so nothing runs and the report is
// the whole output. The name is a client-supplied string repeated into the
// report, so it is clipped rather than trusted; the index, which is what
// identifies the call, is kept whole.
func TestToolCatalogLimitReportBoundsAHostileToolName(t *testing.T) {
	tests := []struct {
		name string
		call string
	}{
		{"ascii", strings.Repeat("n", maxCatalogCallNameCharsForTest)},
		{"multibyte", strings.Repeat("é", maxCatalogCallNameCharsForTest)},
		{"control characters", strings.Repeat("a\u0007\u001b", maxCatalogCallNameCharsForTest/3)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(echoTool()),
				server.WithToolCatalogLimits(25, 512),
			)
			args := fmt.Sprintf(`{"calls":[{"name":%s,"arguments":{}}]}`, mustJSON(tt.call))
			reply := callCatalog(t, s, "execute_tools", args)
			payload := reply.payload

			if got := reply.result["isError"]; got != true {
				t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
			}
			assertWithinBudget(t, reply, 512)

			omitted, ok := payload["omittedToolCall"].(map[string]any)
			if !ok {
				t.Fatalf("payload names no omitted call: %s", mustJSON(payload))
			}
			if got := omitted["index"]; got != float64(0) {
				t.Fatalf("index = %v, want 0", got)
			}
			reported, _ := omitted["name"].(string)
			if reported == tt.call {
				t.Fatalf("the complete %d-character name was repeated into a 512 byte report", len([]rune(tt.call)))
			}
			// What is reported is a prefix of what was sent, marked as clipped,
			// so a client reads a truthful fragment rather than a mangled name.
			if !strings.HasSuffix(reported, "...") {
				t.Fatalf("clipped name = %q, want it marked as clipped", reported)
			}
			if prefix := strings.TrimSuffix(reported, "..."); !strings.HasPrefix(tt.call, prefix) {
				t.Fatalf("clipped name = %q, want a prefix of the name that was sent", reported)
			}
			if !utf8.ValidString(reported) {
				t.Fatalf("clipped name = %q, want it cut on a character boundary", reported)
			}
			if got := payload["attemptedToolCalls"]; got != float64(1) {
				t.Fatalf("attemptedToolCalls = %v, want 1", got)
			}
			if got := payload["reportedToolCalls"]; got != float64(0) {
				t.Fatalf("reportedToolCalls = %v, want 0", got)
			}
		})
	}
}

// maxCatalogCallNameCharsForTest is the longest tool name the execute schema
// accepts. It is restated here rather than read from the package under test, so
// the test fails if the advertised limit and the report's bound drift apart.
const maxCatalogCallNameCharsForTest = 255

func TestToolCatalogExecuteRejectsMalformedBatches(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool()))

	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "calls is missing",
			args: `{}`,
			want: "The calls field is required.",
		},
		{
			name: "calls is empty",
			args: `{"calls":[]}`,
			want: "The calls field is required.",
		},
		{
			name: "calls is an object",
			args: `{"calls":{"name":"say-hi-tool"}}`,
			want: "The calls field must be an array.",
		},
		{
			name: "calls is a string",
			args: `{"calls":"say-hi-tool"}`,
			want: "The calls field must be an array.",
		},
		{
			name: "an entry is not an object",
			args: `{"calls":["say-hi-tool"]}`,
			want: "The calls.0 field must be an object.",
		},
		{
			name: "an entry carries an unknown key",
			args: `{"calls":[{"name":"say-hi-tool","timeout":5}]}`,
			want: "The calls.0 field must only contain the name and arguments keys.",
		},
		{
			name: "a name is missing",
			args: `{"calls":[{"arguments":{}}]}`,
			want: "The calls.0.name field is required.",
		},
		{
			name: "a name is not a string",
			args: `{"calls":[{"name":42}]}`,
			want: "The calls.0.name field must be a string.",
		},
		{
			name: "a name is oversized",
			args: `{"calls":[{"name":"` + strings.Repeat("x", 256) + `"}]}`,
			want: "The calls.0.name field must not exceed 255 characters.",
		},
		{
			name: "arguments is a list",
			args: `{"calls":[{"name":"say-hi-tool","arguments":["Ada"]}]}`,
			want: "The calls.0.arguments field must be an object.",
		},
		{
			name: "arguments is a string",
			args: `{"calls":[{"name":"say-hi-tool","arguments":"Ada"}]}`,
			want: "The calls.0.arguments field must be an object.",
		},
		{
			name: "a later entry is reported with its own index",
			args: `{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}},{"name":""}]}`,
			want: "The calls.1.name field is required.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, _, text := callCatalogTool(t, s, "execute_tools", tc.args)
			if got := result["isError"]; got != true {
				t.Fatalf("isError = %v, want true (text %q)", got, text)
			}
			if text != tc.want {
				t.Errorf("message = %q, want %q", text, tc.want)
			}
		})
	}
}

// TestToolCatalogRejectionsAreHeldToTheBudget covers the one reply a client can
// make arbitrarily long without running anything: a malformed batch is rejected
// with a message naming every bad entry, so the message grows with the batch.
// It is a catalog reply like any other and is held to the same limit.
func TestToolCatalogRejectionsAreHeldToTheBudget(t *testing.T) {
	const budget = 512
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(sayHiTool()),
		server.WithToolCatalogLimits(25, budget),
	)

	entries := make([]string, 25)
	for i := range entries {
		entries[i] = `{"name":1,"arguments":[],"extra":1}`
	}
	reply := callCatalog(t, s, "execute_tools", `{"calls":[`+strings.Join(entries, ",")+`]}`)

	if got := reply.result["isError"]; got != true {
		t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
	}
	assertWithinBudget(t, reply, budget)
	// What the client reads is the start of the same message, marked where it
	// had to be cut so a clipped diagnostic is never read as a complete one.
	want := "The calls.0 field must only contain the name and arguments keys."
	if !strings.HasPrefix(reply.text, want) {
		t.Fatalf("message = %q, want it to begin with %q", reply.text, want)
	}
	if !strings.HasSuffix(reply.text, "...") {
		t.Fatalf("message = %q, want it marked as clipped", reply.text)
	}
}

// TestToolCatalogBudgetCountsTheMetadataChannel pins the limit to the whole
// reply and not only to the part a model reads. A tool addresses its metadata
// to the host, and a batch forwards it in the reply's metadata channel, so a
// limit that counted content alone would let any tool push as much past it as
// it liked by writing there instead.
func TestToolCatalogBudgetCountsTheMetadataChannel(t *testing.T) {
	annotated := server.NewTool("annotated-tool", "Addresses a payload to the host").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			// Markup in the metadata costs the same six bytes a character on
			// the wire as markup in the content does.
			return server.Text("ok").WithMeta("trace", strings.Repeat("<b>&</b>", 150)), nil
		})
	arguments := `{"calls":[{"name":"annotated-tool","arguments":{}}]}`
	budget := measureReply(t, []server.Tool{annotated}, "execute_tools", arguments)

	t.Run("exactly the budget fits", func(t *testing.T) {
		s := server.New("demo", "1.0.0",
			server.WithToolCatalog(annotated),
			server.WithToolCatalogLimits(25, budget),
		)
		reply := callCatalog(t, s, "execute_tools", arguments)
		if got := reply.result["isError"]; got != false {
			t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
		}
		meta, _ := reply.result["_meta"].(map[string]any)
		if _, ok := meta["com.velocitykode.mcp/toolCatalog"]; !ok {
			t.Fatalf("the measured reply carries no catalog metadata: %s", mustJSON(reply.result))
		}
		if got := replySize(t, reply.wire); got != budget {
			t.Fatalf("the reply is %d bytes, want the %d it measured", got, budget)
		}
	})

	t.Run("one byte less does not", func(t *testing.T) {
		s := server.New("demo", "1.0.0",
			server.WithToolCatalog(annotated),
			server.WithToolCatalogLimits(25, budget-1),
		)
		reply := callCatalog(t, s, "execute_tools", arguments)
		if got := reply.result["isError"]; got != true {
			t.Fatalf("isError = %v, want true (text %q)", got, reply.text)
		}
		want := map[string]any{
			"ok":       false,
			"complete": false,
			"error": map[string]any{
				"kind":    "OutputLimitExceeded",
				"message": fmt.Sprintf("The tool output exceeded %d bytes.", budget-1),
			},
			"results":            []any{},
			"omittedToolCall":    map[string]any{"name": "annotated-tool", "index": float64(0)},
			"requestedToolCalls": float64(1),
			"attemptedToolCalls": float64(1),
			"reportedToolCalls":  float64(0),
		}
		if !jsonEqual(reply.payload, want) {
			t.Fatalf("payload = %s, want %s", mustJSON(reply.payload), mustJSON(want))
		}
		assertWithinBudget(t, reply, budget-1)
	})
}

// TestToolCatalogBudgetExcludesTheReplyEnvelope states, in bytes, what the
// output limit covers and what it does not. The catalog measures the tool
// result it builds, which is all it can see; the server then writes the result
// type and its own implementation metadata into that result on its way out. So
// the result member a client receives is the configured limit plus a fixed
// overhead, and a host sizing a context window can add the two rather than
// guess at the difference.
//
// The overhead is stated here from the members themselves, not read back from
// what the server produced, so a change to either side has to be made here too.
func TestToolCatalogBudgetExcludesTheReplyEnvelope(t *testing.T) {
	// The two members the reply envelope adds to a tools/call result, as the
	// result shape and this server's own name and version define them. A
	// tools/call carries no caching hints: it is not a cacheable operation.
	const (
		resultTypeMember = `"resultType":"complete"`
		serverInfoMember = `"io.modelcontextprotocol/serverInfo":{"name":"demo","version":"1.0.0"}`
	)
	// Each member costs its own text plus the comma separating it from the
	// members already there. A reply that carries metadata of its own pays for
	// the server info alone, since the _meta object it is written into is
	// already on the wire; a reply that carries none pays for that object too.
	withMeta := len(",") + len(resultTypeMember) + len(",") + len(serverInfoMember)
	withoutMeta := withMeta + len(`"_meta":{`) + len(`}`)
	if withMeta != 95 || withoutMeta != 105 {
		t.Fatalf("envelope overhead = %d bytes with metadata and %d without, want 95 and 105", withMeta, withoutMeta)
	}

	annotated := server.NewTool("annotated-tool", "Addresses a payload to the host").
		HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
			return server.Text("ok").WithMeta("trace", "abc"), nil
		})

	tests := []struct {
		name      string
		tools     []server.Tool
		tool      string
		arguments string
		overhead  int
	}{
		{
			name:      "a reply that carries no metadata of its own",
			tools:     []server.Tool{echoTool()},
			tool:      "execute_tools",
			arguments: `{"calls":[{"name":"echo-tool","arguments":{"value":"hello"}}]}`,
			overhead:  withoutMeta,
		},
		{
			name:      "a reply that forwards the metadata of an inner call",
			tools:     []server.Tool{annotated},
			tool:      "execute_tools",
			arguments: `{"calls":[{"name":"annotated-tool","arguments":{}}]}`,
			overhead:  withMeta,
		},
		{
			name:      "a search reply",
			tools:     []server.Tool{echoTool()},
			tool:      "search_tools",
			arguments: `{}`,
			overhead:  withoutMeta,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The budget is the reply's own size, so the catalog is held to
			// exactly what it measured and the remainder is the envelope.
			budget := measureReply(t, tc.tools, tc.tool, tc.arguments)
			s := server.New("demo", "1.0.0",
				server.WithToolCatalog(tc.tools...),
				server.WithToolCatalogLimits(25, budget),
			)
			reply := callCatalog(t, s, tc.tool, tc.arguments)
			if got := reply.result["isError"]; got != false {
				t.Fatalf("isError = %v, want false (text %q)", got, reply.text)
			}

			if got := replySize(t, reply.wire); got != budget {
				t.Fatalf("the catalog produced %d bytes under a %d byte limit", got, budget)
			}
			if got, want := len(reply.wire), budget+tc.overhead; got != want {
				t.Fatalf("the result member is %d bytes, want the %d byte limit plus %d bytes of envelope: %s",
					got, budget, tc.overhead, reply.wire)
			}
		})
	}
}

func TestToolCatalogExecuteReportsEveryBadEntry(t *testing.T) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool()))

	// Both entries are malformed: the batch is rejected once, naming each.
	_, _, text := callCatalogTool(t, s, "execute_tools", `{"calls":[{"name":42},{"name":"say-hi-tool","arguments":[]}]}`)
	want := "The calls.0.name field must be a string. The calls.1.arguments field must be an object."
	if text != want {
		t.Fatalf("message = %q, want %q", text, want)
	}
}

func TestToolCatalogEnforcesCallAndOutputLimits(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithToolCatalog(sayHiTool()),
		server.WithToolCatalogLimits(1, 512),
	)

	t.Run("the batch size is capped", func(t *testing.T) {
		result, _, text := callCatalogTool(t, s, "execute_tools",
			`{"calls":[{"name":"say-hi-tool","arguments":{"name":"One"}},{"name":"say-hi-tool","arguments":{"name":"Two"}}]}`)
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true", got)
		}
		if want := "The calls field must not have more than 1 items."; text != want {
			t.Errorf("message = %q, want %q", text, want)
		}
	})

	t.Run("the cap is advertised in the schema", func(t *testing.T) {
		listed := decodeResult(t, handle(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Response)
		tools, _ := listed["tools"].([]any)
		execute, _ := tools[1].(map[string]any)
		input, _ := execute["inputSchema"].(map[string]any)
		properties, _ := input["properties"].(map[string]any)
		calls, _ := properties["calls"].(map[string]any)
		if got := calls["maxItems"]; got != float64(1) {
			t.Errorf("calls maxItems = %v, want 1", got)
		}
		if got := calls["minItems"]; got != float64(1) {
			t.Errorf("calls minItems = %v, want 1", got)
		}
	})

	t.Run("an oversized result reports the limit and the call counts", func(t *testing.T) {
		result, payload, text := callCatalogTool(t, s, "execute_tools",
			`{"calls":[{"name":"say-hi-tool","arguments":{"name":"`+strings.Repeat("x", 256)+`"}}]}`)
		if got := result["isError"]; got != true {
			t.Fatalf("isError = %v, want true (text %q)", got, text)
		}
		want := map[string]any{
			"ok":       false,
			"complete": false,
			"error": map[string]any{
				"kind":    "OutputLimitExceeded",
				"message": "The tool output exceeded 512 bytes.",
			},
			"results":            []any{},
			"omittedToolCall":    map[string]any{"name": "say-hi-tool", "index": float64(0)},
			"requestedToolCalls": float64(1),
			"attemptedToolCalls": float64(1),
			"reportedToolCalls":  float64(0),
		}
		if !jsonEqual(payload, want) {
			t.Fatalf("payload = %s, want %s", mustJSON(payload), mustJSON(want))
		}
	})
}

func TestToolCatalogLimitsAreClamped(t *testing.T) {
	s := server.New("demo", "1.0.0",
		server.WithToolCatalogLimits(0, 0),
		server.WithToolCatalog(sayHiTool()),
	)

	// A zero call cap would disable the tool; it is raised to the floor of one.
	result, _, text := callCatalogTool(t, s, "execute_tools",
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":"A"}},{"name":"say-hi-tool","arguments":{"name":"B"}}]}`)
	if got := result["isError"]; got != true {
		t.Fatalf("isError = %v, want true", got)
	}
	if want := "The calls field must not have more than 1 items."; text != want {
		t.Errorf("message = %q, want %q", text, want)
	}

	// A zero byte cap is raised to the floor of 512, which the message reports.
	_, _, text = callCatalogTool(t, s, "execute_tools",
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":"`+strings.Repeat("x", 1024)+`"}}]}`)
	if !strings.Contains(text, "The tool output exceeded 512 bytes.") {
		t.Errorf("message = %q, want it to report the 512 byte floor", text)
	}

	// The catalog option applied after the limits option still registers, and
	// the meta tools sit where the first catalog option put them.
	if got, want := toolNames(t, s), []string{"search_tools", "execute_tools"}; !equalStrings(got, want) {
		t.Errorf("tools/list names = %v, want %v", got, want)
	}
}

func TestToolCatalogInnerCallMatchesDirectCall(t *testing.T) {
	// The same tool is registered twice: once normally, once in the catalog.
	// A batch call must produce the same result a direct tools/call does, so
	// the two invocation paths cannot drift apart.
	catalog := server.New("demo", "1.0.0", server.WithToolCatalog(structuredTool(), sayHiTool()))
	direct := server.New("demo", "1.0.0", server.WithTools(structuredTool(), sayHiTool()))

	tests := []struct {
		name      string
		tool      string
		arguments string
	}{
		{name: "structured content", tool: "structured-content-tool", arguments: `{}`},
		{name: "plain text", tool: "say-hi-tool", arguments: `{"name":"Ada"}`},
		{name: "validation failure", tool: "say-hi-tool", arguments: `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			directResult, _, _ := callCatalogTool(t, direct, tc.tool, tc.arguments)

			batchResult, payload, _ := callCatalogTool(t, catalog, "execute_tools",
				fmt.Sprintf(`{"calls":[{"name":%q,"arguments":%s}]}`, tc.tool, tc.arguments))
			results := batchResults(t, batchResult, payload)
			if len(results) != 1 {
				t.Fatalf("results = %s, want one entry", mustJSON(payload["results"]))
			}
			inner := results[0]

			// The result envelope decorates a JSON-RPC reply, so it belongs to
			// the outer result on both paths and never to a call reassembled
			// from a batch, which is one call's share of that reply rather than
			// a reply of its own. Both halves are asserted so neither the
			// envelope nor the catalog can start writing it in the wrong place
			// unnoticed.
			assertResultEnvelope(t, "direct result", directResult)
			assertResultEnvelope(t, "batch result", batchResult)
			for _, key := range []string{"resultType", "_meta"} {
				if _, ok := inner[key]; ok {
					t.Errorf("reassembled call carries %q: %s", key, mustJSON(inner))
				}
			}

			if !jsonEqual(inner, withoutResultEnvelope(directResult)) {
				t.Fatalf("batch result = %s, direct result = %s", mustJSON(inner), mustJSON(directResult))
			}
		})
	}
}

// assertResultEnvelope checks that a JSON-RPC result carries the two members
// the server decorates every successful reply with.
func assertResultEnvelope(t *testing.T, label string, result map[string]any) {
	t.Helper()
	if got := result["resultType"]; got != "complete" {
		t.Errorf("%s resultType = %v, want complete", label, got)
	}
	meta, _ := result["_meta"].(map[string]any)
	info, _ := meta[server.MetaKeyServerInfo].(map[string]any)
	if got := info["name"]; got != "demo" {
		t.Errorf("%s _meta[%s].name = %v, want demo", label, server.MetaKeyServerInfo, got)
	}
}

// withoutResultEnvelope returns a copy of a JSON-RPC result with the members
// the reply envelope owns removed, leaving the tool result a batch entry
// carries. The tools in this test attach no _meta of their own, so dropping the
// key removes only what the envelope wrote.
func withoutResultEnvelope(result map[string]any) map[string]any {
	out := make(map[string]any, len(result))
	for k, v := range result {
		if k == "resultType" || k == "_meta" {
			continue
		}
		out[k] = v
	}
	return out
}

// TestToolCatalogBatchProgress pins the progress a batch reports: the batch is
// one request and owns the client's token, so it counts the calls it ran rather
// than forwarding the sequence of every inner tool. Each inner call of
// "progress-tool" reports 1 then 2 of its own, so forwarding them would restart
// the count at every call and announce the total before the batch is done,
// while the MCP progress notification requires the value to increase with every
// notification sent under one token.
func TestToolCatalogBatchProgress(t *testing.T) {
	tests := []struct {
		name  string
		meta  string
		calls int
		want  []float64
	}{
		{name: "single call", meta: `,"_meta":{"progressToken":"tok-1"}`, calls: 1, want: []float64{1}},
		{name: "batch of calls", meta: `,"_meta":{"progressToken":"tok-1"}`, calls: 3, want: []float64{1, 2, 3}},
		{name: "no progress token", meta: "", calls: 2, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", server.WithToolCatalog(progressTool()))

			var mu sync.Mutex
			var frames []map[string]any
			emit := func(msg []byte) error {
				var frame map[string]any
				if err := json.Unmarshal(msg, &frame); err != nil {
					return err
				}
				mu.Lock()
				frames = append(frames, frame)
				mu.Unlock()
				return nil
			}

			calls := make([]string, tc.calls)
			for i := range calls {
				calls[i] = `{"name":"progress-tool","arguments":{}}`
			}
			raw := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"execute_tools"%s,"arguments":{"calls":[%s]}}}`,
				tc.meta, strings.Join(calls, ","))
			res := s.HandleStream(context.Background(), []byte(raw), "sess-1", emit)

			result := decodeResult(t, res.Response)
			if got := result["isError"]; got != false {
				t.Fatalf("isError = %v, want false", got)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(frames) != len(tc.want) {
				t.Fatalf("emitted %d frames, want %d: %v", len(frames), len(tc.want), frames)
			}
			for i, frame := range frames {
				if got := frame["method"]; got != "notifications/progress" {
					t.Errorf("frame %d method = %v, want notifications/progress", i, got)
				}
				params, _ := frame["params"].(map[string]any)
				if got := params["progressToken"]; got != "tok-1" {
					t.Errorf("frame %d progressToken = %v, want tok-1", i, got)
				}
				if got := params["progress"]; got != tc.want[i] {
					t.Errorf("frame %d progress = %v, want %v", i, got, tc.want[i])
				}
				if got := params["total"]; got != float64(tc.calls) {
					t.Errorf("frame %d total = %v, want %d", i, got, tc.calls)
				}
				if got := params["message"]; got != "Ran [progress-tool]." {
					t.Errorf("frame %d message = %v, want Ran [progress-tool].", i, got)
				}
			}
		})
	}
}

func TestToolCatalogInnerCallInheritsRequestContext(t *testing.T) {
	var (
		mu        sync.Mutex
		sessionID string
		meta      map[string]any
	)
	inspector := server.NewTool("inspect-tool", "Reports the request context it was given").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			mu.Lock()
			sessionID = req.SessionID()
			meta = req.Meta()
			mu.Unlock()
			return server.Text("inspected"), nil
		})
	s := server.New("demo", "1.0.0", server.WithToolCatalog(inspector))

	raw := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"execute_tools","_meta":{"progressToken":"tok-9"},"arguments":{"calls":[{"name":"inspect-tool","arguments":{}}]}}}`
	result := decodeResult(t, s.Handle(context.Background(), []byte(raw), "sess-42").Response)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if sessionID != "sess-42" {
		t.Errorf("inner session id = %q, want sess-42", sessionID)
	}
	if got := meta["progressToken"]; got != "tok-9" {
		t.Errorf("inner _meta progressToken = %v, want tok-9", got)
	}
}

// catalogPerson is the shape an application's user model takes: it satisfies
// the identity contract without this package knowing anything about it.
type catalogPerson struct{ id string }

func (p *catalogPerson) GetAuthIdentifier() any   { return p.id }
func (p *catalogPerson) GetAuthPassword() string  { return "" }
func (p *catalogPerson) GetRememberToken() string { return "" }
func (p *catalogPerson) SetRememberToken(string)  {}

// identityTool reports the identifier of whoever the request authenticated, or
// "anonymous" when the call carries no identity. It is registered under the
// same name on both servers so the two invocation paths can be compared.
func identityTool(scheme string) server.Tool {
	return server.NewTool("whoami-tool", "Reports the authenticated caller").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			var user server.Identity
			if scheme == "" {
				user = req.User()
			} else {
				user = req.User(scheme)
			}
			if user == nil {
				return server.Text("anonymous"), nil
			}
			id, _ := user.GetAuthIdentifier().(string)
			return server.Text(id), nil
		})
}

func TestToolCatalogInnerCallResolvesTheAuthenticatedUser(t *testing.T) {
	// A tool moved behind the catalog must see the same caller it would see on
	// the direct path: the inbound request context carries the resolver, so a
	// batch that did not inherit it would silently downgrade every catalog tool
	// to an anonymous call.
	ada := &catalogPerson{id: "ada"}
	grace := &catalogPerson{id: "grace"}

	resolver := func(name string) server.Identity {
		switch name {
		case "", "web":
			return ada
		case "api":
			return grace
		default:
			return nil
		}
	}

	tests := []struct {
		name   string
		scheme string
		ctx    func() context.Context
		want   string
	}{
		{
			name: "the default scheme answers an unnamed call",
			ctx: func() context.Context {
				return server.WithIdentityResolver(context.Background(), resolver)
			},
			want: "ada",
		},
		{
			name:   "a named scheme resolves its own caller",
			scheme: "api",
			ctx: func() context.Context {
				return server.WithIdentityResolver(context.Background(), resolver)
			},
			want: "grace",
		},
		{
			name:   "an unknown scheme authenticates nobody",
			scheme: "ldap",
			ctx: func() context.Context {
				return server.WithIdentityResolver(context.Background(), resolver)
			},
			want: "anonymous",
		},
		{
			name: "a transport with no request behind it installs no resolver",
			ctx:  func() context.Context { return context.Background() },
			want: "anonymous",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalog := server.New("demo", "1.0.0", server.WithToolCatalog(identityTool(tc.scheme)))
			direct := server.New("demo", "1.0.0", server.WithTools(identityTool(tc.scheme)))

			batch := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_tools","arguments":{"calls":[{"name":"whoami-tool","arguments":{}}]}}}`
			result := decodeResult(t, catalog.Handle(tc.ctx(), []byte(batch), "sess-1").Response)
			if got := result["isError"]; got != false {
				t.Fatalf("isError = %v, want false: %s", got, mustJSON(result))
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(firstText(t, result)), &payload); err != nil {
				t.Fatalf("decode batch payload: %v", err)
			}
			results := batchResults(t, result, payload)
			if len(results) != 1 {
				t.Fatalf("results = %s, want one entry", mustJSON(payload["results"]))
			}
			got := firstText(t, results[0])
			if got != tc.want {
				t.Errorf("catalog call resolved %q, want %q", got, tc.want)
			}

			single := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami-tool","arguments":{}}}`
			directResult := decodeResult(t, direct.Handle(tc.ctx(), []byte(single), "sess-1").Response)
			if want := firstText(t, directResult); got != want {
				t.Errorf("catalog call resolved %q, direct call resolved %q", got, want)
			}
		})
	}
}

func TestToolCatalogRejectsDuplicateNames(t *testing.T) {
	t.Run("two catalog entries sharing a name", func(t *testing.T) {
		s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), sayHiTool()))

		want := "Duplicate tool name [say-hi-tool] in the tool catalog."
		for _, tool := range []struct{ name, args string }{
			{"search_tools", `{}`},
			{"execute_tools", `{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}}]}`},
		} {
			result, _, text := callCatalogTool(t, s, tool.name, tool.args)
			if got := result["isError"]; got != true {
				t.Fatalf("%s isError = %v, want true", tool.name, got)
			}
			if text != want {
				t.Errorf("%s message = %q, want %q", tool.name, text, want)
			}
		}
	})

	t.Run("a registered tool colliding with a generated name", func(t *testing.T) {
		collision := server.NewTool("search_tools", "Shadows the catalog search tool").
			HandleFunc(func(context.Context, *server.Request) (*server.Response, error) {
				return server.Text("collision"), nil
			})
		logger := &countingLogger{}
		s := server.New("demo", "1.0.0",
			server.WithToolCatalog(sayHiTool()),
			server.WithTools(collision),
			server.WithLogger(logger),
		)

		want := "Duplicate server tool name [search_tools]."
		for _, tool := range []struct{ name, args string }{
			{"execute_tools", `{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}}]}`},
			{"search_tools", `{}`},
		} {
			result, _, text := callCatalogTool(t, s, tool.name, tool.args)
			if got := result["isError"]; got != true {
				t.Fatalf("%s isError = %v, want true", tool.name, got)
			}
			if text != want {
				t.Errorf("%s message = %q, want %q", tool.name, text, want)
			}
		}
		if got := logger.errors(); got != 1 {
			t.Errorf("logged %d errors, want 1", got)
		}

		// New cannot fail, so the colliding tool is neither dropped nor hidden:
		// the list still advertises both names and a call resolves to the first
		// registration, which is the meta tool reporting the misconfiguration.
		if got, want := toolNames(t, s), []string{"search_tools", "execute_tools", "search_tools"}; !equalStrings(got, want) {
			t.Errorf("tools/list names = %v, want %v", got, want)
		}
	})
}

func TestToolCatalogHandlesConcurrentCalls(t *testing.T) {
	recorder := &recordingTool{}
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), structuredTool(), recorder))

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan string, workers*2)

	// reply returns the report text of a tools/call response followed by the
	// text of every content item forwarded after it, or a description of what
	// went wrong instead. It avoids the *testing.T helpers, which must not be
	// called from a goroutine.
	reply := func(res server.HandleResult, items int) string {
		if res.Response == nil || res.Response.Error != nil {
			return fmt.Sprintf("bad response %+v", res.Response)
		}
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(res.Response.Result, &result); err != nil {
			return fmt.Sprintf("undecodable result: %v", err)
		}
		if result.IsError || len(result.Content) != items {
			return fmt.Sprintf("error result %s", res.Response.Result)
		}
		parts := make([]string, 0, items)
		for _, item := range result.Content {
			parts = append(parts, item.Text)
		}
		return strings.Join(parts, "\n")
	}

	for i := range workers {
		go func(i int) {
			defer wg.Done()
			search := s.Handle(context.Background(),
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_tools","arguments":{"query":"person"}}}`),
				"sess-1")
			// Every worker searches the same catalog, so every worker must see
			// exactly the one tool whose description mentions a person.
			wantSearch := `{"ok":true,"tools":[{"name":"say-hi-tool","description":"This tool says hello to a person","inputSchema":{"properties":{"name":{"description":"The name of the person to greet","type":"string"}},"required":["name"],"type":"object"}}],"hasMore":false}`
			if got := reply(search, 1); got != wantSearch {
				errs <- fmt.Sprintf("search %d payload = %s", i, got)
			}
			execute := s.Handle(context.Background(),
				[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"execute_tools","arguments":{"calls":[{"name":"recording-tool","arguments":{"value":"v%d"}}]}}}`, i)),
				"sess-1")
			// The report and the one content item the call forwarded, so a
			// worker that read another worker's result would be caught here.
			wantExecute := fmt.Sprintf(
				`{"ok":true,"complete":true,"results":[{"name":"recording-tool","index":0,"isError":false,"contentItems":1}],"requestedToolCalls":1,"attemptedToolCalls":1,"reportedToolCalls":1}`+"\n"+`v%d`, i)
			if got := reply(execute, 2); got != wantExecute {
				errs <- fmt.Sprintf("execute %d payload = %s", i, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for message := range errs {
		t.Error(message)
	}
	if got := len(recorder.calls()); got != workers {
		t.Errorf("recorded %d calls, want %d", got, workers)
	}
}

func FuzzToolCatalogSearchQuery(f *testing.F) {
	s := server.New("demo", "1.0.0", server.WithToolCatalog(sayHiTool(), structuredTool()))
	// The same catalog at the narrowest budget the catalog accepts, so the
	// reply of every query is also weighed against a limit it can barely meet.
	// A tool whose description is markup pays six bytes a character for it on
	// the wire, which is what that limit has to count.
	const budget = 512
	tight := server.New("demo", "1.0.0",
		server.WithToolCatalog(sayHiTool(), structuredTool(), markupTool()),
		server.WithToolCatalogLimits(25, budget),
	)

	for _, seed := range []string{
		"", "person name", "PERSON", "  ", "\x00\x01", "\u00e9t\u00e9", "\u4f60\u597d",
		"a\u2028b", strings.Repeat("x", 5000), "</script>", "\\\"", "say-hi-tool",
		"<b>&</b>", "html-tool",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, query string) {
		arguments, err := json.Marshal(map[string]any{"query": query})
		if err != nil {
			t.Fatalf("encode query: %v", err)
		}
		raw, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "search_tools", "arguments": json.RawMessage(arguments)},
		})
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}

		narrow := tight.Handle(context.Background(), raw, "sess-1")
		if narrow.Response == nil || narrow.Response.Error != nil {
			t.Fatalf("query %q under the floor budget: %+v", query, narrow.Response)
		}
		var narrowResult map[string]any
		if err := json.Unmarshal(narrow.Response.Result, &narrowResult); err != nil {
			t.Fatalf("result is not valid JSON: %v", err)
		}
		// Whatever the query matched, the reply is held to the budget the
		// catalog was configured with, rejection messages included.
		assertWithinBudget(t, catalogReply{wire: narrow.Response.Result, result: narrowResult}, budget)

		res := s.Handle(context.Background(), raw, "sess-1")
		if res.Response == nil {
			t.Fatal("no response")
		}
		if res.Response.Error != nil {
			t.Fatalf("protocol error for query %q: %+v", query, res.Response.Error)
		}
		var result map[string]any
		if err := json.Unmarshal(res.Response.Result, &result); err != nil {
			t.Fatalf("result is not valid JSON: %v", err)
		}
		items, _ := result["content"].([]any)
		if len(items) != 1 {
			t.Fatalf("result carries %d content items, want 1", len(items))
		}
		item, _ := items[0].(map[string]any)
		text, _ := item["text"].(string)

		isError, ok := result["isError"].(bool)
		if !ok {
			t.Fatalf("result carries no isError flag: %s", res.Response.Result)
		}
		if isError {
			// A rejected query is an argument-validation message, not a
			// payload; the only queries that may be rejected are ones longer
			// than the advertised maxLength, measured (like the keyword itself)
			// in characters, since every other string is a valid query.
			if utf8.RuneCountInString(query) <= 4096 {
				t.Fatalf("query %q of %d characters was rejected: %q", query, utf8.RuneCountInString(query), text)
			}
			if want := "The query field must not exceed 4096 characters."; text != want {
				t.Fatalf("rejection message = %q, want %q", text, want)
			}
			return
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			t.Fatalf("payload is not valid JSON: %v (%q)", err, text)
		}
		if got := payload["ok"]; got != true {
			t.Fatalf("payload ok = %v, want true: %q", got, text)
		}
		if _, ok := payload["tools"].([]any); !ok {
			t.Fatalf("payload carries no tools array: %q", text)
		}
		if _, ok := payload["hasMore"].(bool); !ok {
			t.Fatalf("payload carries no hasMore flag: %q", text)
		}
	})
}

func FuzzToolCatalogExecuteBatch(f *testing.F) {
	// The budget is the floor, the narrowest the catalog can be configured to,
	// so every reply this target produces is one the catalog had to hold to a
	// limit it could barely meet.
	const budget = 512
	s := server.New("demo", "1.0.0",
		// The echo tool lets an input drive the size of the reply, so the
		// budget invariant below is reachable from the corpus.
		server.WithToolCatalog(sayHiTool(), structuredTool(), echoTool()),
		server.WithToolCatalogLimits(25, budget),
	)

	for _, seed := range []string{
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":"Ada"}}]}`,
		`{"calls":[]}`,
		`{"calls":null}`,
		`{"calls":[{}]}`,
		`{"calls":[{"name":"say-hi-tool","arguments":[]}]}`,
		"{\"calls\":[{\"name\":\"\\u0000\"}]}",
		`{"calls":[{"name":"say-hi-tool","extra":1}]}`,
		`{"calls":[[]]}`,
		`{"calls":{"name":"x"}}`,
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":{"nested":true}}}]}`,
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":2e308}}]}`,
		`""`,
		`[]`,
		`{"calls":[{"name":"echo-tool","arguments":{"value":"` + strings.Repeat("a", 380) + `"}}]}`,
		`{"calls":[{"name":"echo-tool","arguments":{"value":"` + strings.Repeat("a", 2000) + `"}}]}`,
		`{"calls":[{"name":"echo-tool","arguments":{"value":"a"}},{"name":"echo-tool","arguments":{"value":"` + strings.Repeat("b", 400) + `"}}]}`,
		`{"calls":[{"name":"say-hi-tool","arguments":{"name":"padded"}},{"name":"structured-content-tool"}]}`,
		// A batch of malformed entries: the rejection names every one of them,
		// so the message grows with the batch and the reply that carries it has
		// to be held to the budget like any other.
		`{"calls":[{"name":1,"arguments":[],"extra":1},{"name":1,"arguments":[],"extra":1},{"name":1,"arguments":[],"extra":1},{"name":1,"arguments":[],"extra":1}]}`,
		`{"calls":[{"name":1},{"name":1},{"name":1},{"name":1},{"name":1},{"name":1},{"name":1},{"name":1}]}`,
		// Markup costs six bytes a character once serialized, so an echoed
		// value of it is far larger on the wire than in the payload text.
		`{"calls":[{"name":"echo-tool","arguments":{"value":"` + strings.Repeat("<", 120) + `"}}]}`,
		`{"calls":[{"name":"echo-tool","arguments":{"value":"a"}},{"name":"echo-tool","arguments":{"value":"` + strings.Repeat("&", 120) + `"}}]}`,
		// A name spent entirely on characters the report pays six bytes each
		// for, addressing no tool at all.
		`{"calls":[{"name":"` + strings.Repeat(`\u0007`, maxCatalogCallNameCharsForTest) + `"}]}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, arguments string) {
		if !json.Valid([]byte(arguments)) {
			// Nothing to drive: the request would not even be a JSON frame.
			return
		}
		// Syntactically valid JSON can still be undecodable: a number outside
		// the float64 range, for example, has no Go value to hold it. The
		// params object of such a request never decodes, so the request is
		// rejected by the method layer before the catalog ever sees it, as is
		// an "arguments" member that is not an object at all.
		var decoded any
		decodable := json.Unmarshal([]byte(arguments), &decoded) == nil
		_, isObject := decoded.(map[string]any)
		reachesCatalog := decodable && isObject

		raw, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "execute_tools", "arguments": json.RawMessage(arguments)},
		})
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}

		res := s.Handle(context.Background(), raw, "sess-1")
		if res.Response == nil {
			t.Fatal("no response")
		}
		if res.Response.Error != nil {
			// Only params the method layer refuses may surface as a protocol
			// error; every batch the catalog can be handed, however badly
			// shaped, must come back as a tool error result.
			if reachesCatalog {
				t.Fatalf("protocol error for arguments %q: %+v", arguments, res.Response.Error)
			}
			if got := res.Response.Error.Code; got != jsonrpc.CodeInvalidParams {
				t.Fatalf("rejected params gave error code %d, want %d", got, jsonrpc.CodeInvalidParams)
			}
			return
		}
		var result map[string]any
		if err := json.Unmarshal(res.Response.Result, &result); err != nil {
			t.Fatalf("result is not valid JSON: %v", err)
		}
		if _, ok := result["isError"].(bool); !ok {
			t.Fatalf("result carries no isError flag: %s", res.Response.Result)
		}
		// Whatever the batch was, the reply is held to the budget it was
		// served under, diagnostics and rejection messages included.
		assertWithinBudget(t, catalogReply{wire: res.Response.Result, result: result}, budget)
		assertReportAccountsForEveryCall(t, result)
	})
}

// assertReportAccountsForEveryCall checks the bookkeeping of an execute report,
// whatever outcome produced it: a batch must always say which of the calls it
// was given ran and which of those it is carrying a result for.
//
// A reply whose text is not an execute report (an argument rejection, say) is
// nothing to check and passes.
func assertReportAccountsForEveryCall(t *testing.T, result map[string]any) {
	t.Helper()
	items, _ := result["content"].([]any)
	if len(items) == 0 {
		return
	}
	item, _ := items[0].(map[string]any)
	text, _ := item["text"].(string)
	var payload map[string]any
	if json.Unmarshal([]byte(text), &payload) != nil {
		return
	}
	requested, ok := payload["requestedToolCalls"].(float64)
	if !ok {
		return
	}
	attempted, ok := payload["attemptedToolCalls"].(float64)
	if !ok {
		t.Fatalf("the report states no attempted count: %s", text)
	}
	reported, ok := payload["reportedToolCalls"].(float64)
	if !ok {
		t.Fatalf("the report states no reported count: %s", text)
	}
	records, ok := payload["results"].([]any)
	if !ok {
		t.Fatalf("the report carries no results array: %s", text)
	}

	if !(requested >= attempted && attempted >= reported) {
		t.Fatalf("counters run backwards (%v requested, %v attempted, %v reported): %s",
			requested, attempted, reported, text)
	}
	if float64(len(records)) != reported {
		t.Fatalf("the report states %v records and carries %d: %s", reported, len(records), text)
	}
	// The records answer the first calls of the batch, in order, so a client
	// knows from the counters alone which calls it has yet to reissue.
	for i, raw := range records {
		record, _ := raw.(map[string]any)
		if got := record["index"]; got != float64(i) {
			t.Fatalf("record %d answers call %v: %s", i, got, text)
		}
	}
	// A call that ran without a record here is the one named as omitted, and
	// there is at most one of those: the batch stops at the first result it
	// cannot carry.
	omitted, hasOmitted := payload["omittedToolCall"].(map[string]any)
	if attempted-reported > 1 {
		t.Fatalf("%v calls ran without a record: %s", attempted-reported, text)
	}
	if hasOmitted != (attempted-reported == 1) {
		t.Fatalf("omittedToolCall is present = %v with %v calls unreported: %s", hasOmitted, attempted-reported, text)
	}
	if hasOmitted {
		if got := omitted["index"]; got != attempted-1 {
			t.Fatalf("the omitted call is number %v, want %v: %s", got, attempted-1, text)
		}
	}
	// The flag a host reads instead of the counters says exactly what they say.
	want := requested == attempted && reported == attempted
	if got := payload["complete"]; got != want {
		t.Fatalf("complete = %v, want %v for %v requested, %v attempted, %v reported: %s",
			got, want, requested, attempted, reported, text)
	}
}

// countingLogger counts the errors logged by the server.
type countingLogger struct {
	mu    sync.Mutex
	count int
}

func (l *countingLogger) Debug(string, ...any) {}
func (l *countingLogger) Info(string, ...any)  {}
func (l *countingLogger) Warn(string, ...any)  {}
func (l *countingLogger) Fatal(string, ...any) {}
func (l *countingLogger) Error(string, ...any) {
	l.mu.Lock()
	l.count++
	l.mu.Unlock()
}

func (l *countingLogger) errors() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// equalStrings reports whether two string slices carry the same values in the
// same order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// jsonEqual compares two decoded JSON values by their canonical encoding.
func jsonEqual(a, b any) bool {
	return string(mustJSON(a)) == string(mustJSON(b))
}

// mustJSON renders a value as JSON for comparison and failure messages.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf("%q", err.Error()))
	}
	return b
}
