package mcptest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/mcptest"
	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
	_ "github.com/velocitykode/velocity-mcp/server/methods" // installs the full method set
)

// The fixtures below back the assertions added in this file's tests: a tool that
// returns structured content, a tool that streams progress notifications, a
// prompt and a templated resource that both supply completions, and a titled
// tool. assertServer registers them all and opts into the completions
// capability, which completion/complete gates on.

// reportTool returns human-readable text plus a structuredContent object.
func reportTool() server.Tool {
	return server.NewTool("report", "Summarise the run").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text("2 passed, 1 failed").WithStructuredContent(map[string]any{
				"passed": 2,
				"failed": 1,
				"tags":   []string{"unit", "race"},
			}), nil
		})
}

// booking is the typed shape a tool may return as structured content. The
// structured assertions serialize an expectation before comparing, so a test may
// state it with this type rather than restating it as a map.
type booking struct {
	Status string  `json:"status"`
	Amount float64 `json:"amount"`
}

// bookingTool returns structured content whose values a caller would naturally
// express with the booking type above.
func bookingTool() server.Tool {
	return server.NewTool("booking", "Fetch a booking").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text("booking confirmed").WithStructuredContent(map[string]any{
				"status": "confirmed",
				"amount": 10.0,
			}), nil
		})
}

// ledgerTool returns a record id beyond the range float64 represents exactly
// (2^53), which is the everyday case for snowflake and database ids in
// structured tool output.
func ledgerTool() server.Tool {
	return server.NewTool("ledger", "Fetch a ledger entry").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text("entry").WithStructuredContent(map[string]any{
				"id": int64(9007199254740993),
			}), nil
		})
}

// upstreamTool hands back the bytes of an upstream reply as its structured
// content, which is what a tool proxying another API produces: encoding/json
// compacts a json.RawMessage but does not re-spell its numbers, so the wire
// carries 10.0 and 1e2 exactly as the upstream wrote them.
func upstreamTool() server.Tool {
	return server.NewTool("upstream", "Proxy an upstream reply").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Text("proxied").WithStructuredContent(map[string]any{
				"amount": json.RawMessage(`10.0`),
				"ratio":  json.RawMessage(`1e2`),
				"id":     json.RawMessage(`9007199254740993`),
			}), nil
		})
}

// progressTool emits two progress notifications before returning its result. The
// notifications only reach the client when the caller supplies a progressToken
// in the request _meta, which is what the notification assertions exercise.
func progressTool() server.Tool {
	return server.NewTool("import", "Import records").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			if err := req.ReportProgress(server.ProgressUpdate{Progress: 1, Total: 2, Message: "half"}); err != nil {
				return nil, err
			}
			if err := req.ReportProgress(server.ProgressUpdate{Progress: 2, Total: 2, Message: "done"}); err != nil {
				return nil, err
			}
			return server.Text("imported"), nil
		})
}

// impostorTool is registered, and its handler answers with the very text the
// server writes when a tools/call names a tool it does not have. The reply is a
// tool error result, so it states that this handler ran, which is the opposite
// of what the registration assertions claim.
func impostorTool() server.Tool {
	return server.NewTool("impostor", "Answer like a missing tool").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Error("Tool [impostor] not found."), nil
		})
}

// silentFailureTool fails without saying anything: the result carries the
// isError flag and no content at all. The specification makes the flag the
// statement that the call failed, so the error assertions must read this reply
// as an error even though no message can be extracted from it.
func silentFailureTool() server.Tool {
	return server.NewTool("silent-failure", "Fail without content").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.NewResponse().AsError(), nil
		})
}

// blankFailureTool is the same failure one step along: an error result whose
// single text item is empty. The content item exists on the wire but yields no
// message, so a reply read through its messages alone would again pass as
// untroubled.
func blankFailureTool() server.Tool {
	return server.NewTool("blank-failure", "Fail with an empty message").
		HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
			return server.Error(""), nil
		})
}

// titledTool declares an explicit title distinct from its name, so the title
// assertion is not merely reading back a name-derived headline.
type titledTool struct{}

func (titledTool) Name() string          { return "ship-it" }
func (titledTool) Title() string         { return "Ship It Now" }
func (titledTool) Description() string   { return "Ship the build" }
func (titledTool) Schema(*schema.Object) {}
func (titledTool) Handle(_ context.Context, _ *server.Request) (*server.Response, error) {
	return server.Text("shipped"), nil
}

// searchPrompt supplies completions for its "language" argument and nothing for
// any other argument.
type searchPrompt struct{}

func (searchPrompt) Name() string        { return "search" }
func (searchPrompt) Description() string { return "Search the docs" }
func (searchPrompt) Arguments() []server.PromptArgument {
	return []server.PromptArgument{server.NewPromptArgument("language", "Language", false)}
}
func (searchPrompt) Handle(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("searching " + req.String("language")), nil
}
func (searchPrompt) Complete(_ context.Context, req server.CompletionRequest) (server.Completion, error) {
	if req.Argument != "language" {
		return server.Completion{}, nil
	}
	return server.CompleteValues(req.Value, []string{"go", "gleam", "rust"}), nil
}

// docResource is a templated resource that completes its "slug" variable.
type docResource struct{}

func (docResource) Name() string        { return "doc" }
func (docResource) Description() string { return "A documentation page" }
func (docResource) URI() string         { return "file://docs/{slug}" }
func (docResource) URITemplate() string { return "file://docs/{slug}" }
func (docResource) MimeType() string    { return "text/markdown" }
func (docResource) Read(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.NewResponse(content.NewText("doc " + req.String("slug"))), nil
}
func (docResource) Complete(_ context.Context, req server.CompletionRequest) (server.Completion, error) {
	if req.Argument != "slug" {
		return server.Completion{}, nil
	}
	return server.CompleteValues(req.Value, []string{"intro", "install", "internals"}), nil
}

// logResource and apiResource are the second static resource and the second
// resource template of the paginated server below, so each list method has more
// than one page to walk.
type logResource struct{}

func (logResource) Name() string        { return "log" }
func (logResource) Description() string { return "The run log" }
func (logResource) URI() string         { return "file://run.log" }
func (logResource) MimeType() string    { return "text/plain" }
func (logResource) Read(_ context.Context, _ *server.Request) (*server.Response, error) {
	return server.NewResponse(content.NewText("log line")), nil
}

type apiResource struct{}

func (apiResource) Name() string        { return "api" }
func (apiResource) Description() string { return "An API reference page" }
func (apiResource) URI() string         { return "file://api/{version}" }
func (apiResource) URITemplate() string { return "file://api/{version}" }
func (apiResource) MimeType() string    { return "text/markdown" }
func (apiResource) Read(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.NewResponse(content.NewText("api " + req.String("version"))), nil
}

func assertServer() *server.Server {
	return server.New("asserts", "1.0.0",
		server.WithTools(addTool(), boomTool(), reportTool(), progressTool(), titledTool{},
			bookingTool(), ledgerTool(), upstreamTool(), impostorTool(),
			silentFailureTool(), blankFailureTool()),
		server.WithResources(greetingResource{}, docResource{}),
		server.WithPrompts(echoPrompt{}, searchPrompt{}),
		server.WithCapability(server.CapabilityCompletions),
	)
}

// bulkPrompt completes its "tag" argument from more candidates than one
// completion/complete result may carry, so the reply truncates its values while
// reporting the full match count and setting hasMore. It stands on its own
// server: it is the only shape in which the count, the total and the hasMore
// flag disagree, which is what tells the three assertions apart.
type bulkPrompt struct{}

func (bulkPrompt) Name() string        { return "bulk" }
func (bulkPrompt) Description() string { return "Tag in bulk" }
func (bulkPrompt) Arguments() []server.PromptArgument {
	return []server.PromptArgument{server.NewPromptArgument("tag", "Tag", false)}
}
func (bulkPrompt) Handle(_ context.Context, _ *server.Request) (*server.Response, error) {
	return server.Text("tagged"), nil
}
func (bulkPrompt) Complete(_ context.Context, req server.CompletionRequest) (server.Completion, error) {
	if req.Argument != "tag" {
		return server.Completion{}, nil
	}
	tags := make([]string, 0, server.MaxCompletionValues+5)
	for i := 0; i < server.MaxCompletionValues+5; i++ {
		tags = append(tags, fmt.Sprintf("tag-%03d", i))
	}
	return server.CompleteValues(req.Value, tags), nil
}

func bulkServer() *server.Server {
	return server.New("bulk", "1.0.0",
		server.WithPrompts(bulkPrompt{}),
		server.WithCapability(server.CapabilityCompletions),
	)
}

// taskPrompt completes its "taskId" argument from the sibling "projectId" the
// client has already resolved, which reaches the handler as the completion
// context (params.context.arguments). It is the only fixture whose candidate set
// depends on that context, so it is what tells a driver that sends the resolved
// arguments from one that drops them: without the context there is nothing to
// complete, and each resolved project yields its own tasks.
type taskPrompt struct{}

func (taskPrompt) Name() string        { return "task" }
func (taskPrompt) Description() string { return "Pick a task" }
func (taskPrompt) Arguments() []server.PromptArgument {
	return []server.PromptArgument{
		server.NewPromptArgument("projectId", "Project", true),
		server.NewPromptArgument("taskId", "Task", false),
	}
}
func (taskPrompt) Handle(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.Text("task " + req.String("taskId")), nil
}
func (taskPrompt) Complete(_ context.Context, req server.CompletionRequest) (server.Completion, error) {
	switch req.Argument {
	case "projectId":
		return server.CompleteValues(req.Value, []string{"project-1", "project-2"}), nil
	case "taskId":
		switch req.Context["projectId"] {
		case "project-1":
			return server.CompleteValues(req.Value, []string{"task-1-1", "task-1-2"}), nil
		case "project-2":
			return server.CompleteValues(req.Value, []string{"task-2-1", "task-2-2"}), nil
		}
	}
	return server.Completion{}, nil
}

// fileResource is the resource-template half of the same shape: its "fileId"
// variable completes from the resolved "userId".
type fileResource struct{}

func (fileResource) Name() string        { return "file" }
func (fileResource) Description() string { return "A file of a user" }
func (fileResource) URI() string         { return "file://users/{userId}/files/{fileId}" }
func (fileResource) URITemplate() string { return "file://users/{userId}/files/{fileId}" }
func (fileResource) MimeType() string    { return "text/plain" }
func (fileResource) Read(_ context.Context, req *server.Request) (*server.Response, error) {
	return server.NewResponse(content.NewText("file " + req.String("fileId"))), nil
}
func (fileResource) Complete(_ context.Context, req server.CompletionRequest) (server.Completion, error) {
	if req.Argument != "fileId" {
		return server.Completion{}, nil
	}
	switch req.Context["userId"] {
	case "user-1":
		return server.CompleteValues(req.Value, []string{"file1.txt", "file2.txt"}), nil
	case "user-2":
		return server.CompleteValues(req.Value, []string{"doc1.txt", "doc2.txt"}), nil
	}
	return server.Completion{}, nil
}

// contextServer stands on its own so the catalogue counts asserted against
// assertServer stay the statement they are.
func contextServer() *server.Server {
	return server.New("context", "1.0.0",
		server.WithPrompts(taskPrompt{}),
		server.WithResources(fileResource{}),
		server.WithCapability(server.CapabilityCompletions),
	)
}

// paginatedServer registers two of every primitive behind a page size of one, so
// a driver that read only the first page would see exactly half of each
// catalogue.
func paginatedServer() *server.Server {
	return server.New("paginated", "1.0.0",
		server.WithPageSize(1),
		server.WithTools(addTool(), titledTool{}),
		server.WithResources(greetingResource{}, logResource{}, docResource{}, apiResource{}),
		server.WithPrompts(echoPrompt{}, searchPrompt{}),
	)
}

// manyToolsServer registers n tools named tool-0 ... tool-(n-1) at the server's
// default page size, the shape a real consumer has (velship-mcp registers 30
// tools against a default page size of 15).
func manyToolsServer(n int) *server.Server {
	tools := make([]server.Tool, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool-%d", i)
		tools = append(tools, server.NewTool(name, "Tool "+name).
			HandleFunc(func(_ context.Context, _ *server.Request) (*server.Response, error) {
				return server.Text(name), nil
			}))
	}
	return server.New("many", "1.0.0", server.WithTools(tools...))
}

// callWithProgressToken drives tools/call with a progressToken in _meta, which
// is the only way a handler's ReportProgress reaches the client.
func callWithProgressToken(ts *mcptest.Server, tool, token string) *mcptest.Response {
	return ts.Send("tools/call", map[string]any{
		"name":      tool,
		"arguments": map[string]any{},
		"_meta":     map[string]any{"progressToken": token},
	})
}

func TestListAssertions(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	ts.ListResources().
		AssertOk().
		AssertResourceListed("greeting").
		AssertResourceNotListed("doc", "ghost").
		AssertResourceCount(1)

	ts.ListPrompts().
		AssertOk().
		AssertPromptListed("echo").
		AssertPromptListed("search").
		AssertPromptNotListed("ghost").
		AssertPromptCount(2)

	ts.ListResourceTemplates().
		AssertOk().
		AssertResourceTemplateListed("doc").
		AssertResourceTemplateNotListed("greeting", "ghost").
		AssertResourceTemplateCount(1)
}

// TestListAssertions_FollowPagination pins that the list drivers walk every page
// before an assertion runs: with a page size of one, each catalogue arrives in
// two pages, and a driver that stopped at the first would list half of it.
func TestListAssertions_FollowPagination(t *testing.T) {
	ts := mcptest.NewServer(t, paginatedServer())
	ts.Initialize().AssertOk()

	// The wire really is paginated: one page carries a single entry and a cursor
	// for the rest. Without this the merged assertions below would be vacuous.
	page := ts.Send("tools/list", map[string]any{})
	page.AssertOk().AssertToolCount(1)
	if next, _ := page.Result()["nextCursor"].(string); next == "" {
		t.Fatal("expected a single page to carry a nextCursor")
	}

	ts.ListTools().
		AssertOk().
		AssertToolListed("add", "ship-it").
		AssertToolNotListed("ghost").
		AssertToolCount(2)
	if _, present := ts.ListTools().Result()["nextCursor"]; present {
		t.Fatal("a fully walked catalogue must not carry a leftover cursor")
	}

	ts.ListResources().
		AssertOk().
		AssertResourceListed("greeting", "log").
		AssertResourceNotListed("doc", "api", "ghost").
		AssertResourceCount(2)

	ts.ListPrompts().
		AssertOk().
		AssertPromptListed("echo", "search").
		AssertPromptNotListed("ghost").
		AssertPromptCount(2)

	ts.ListResourceTemplates().
		AssertOk().
		AssertResourceTemplateListed("doc", "api").
		AssertResourceTemplateNotListed("greeting", "ghost").
		AssertResourceTemplateCount(2)

	// Entry lookups reach the later pages too, not just the first.
	ts.ListTools().Tool("ship-it").AssertTitle("Ship It Now")
	ts.ListResourceTemplates().ResourceTemplate("api").AssertDescription("An API reference page")
}

// TestListAssertions_BeyondDefaultPage is the case a consumer hits without
// configuring anything: more primitives than the server's default page size. A
// not-registered assertion that silently passed for a registered tool would be
// the worst failure this package could have, so it is pinned down its failing
// path as well.
func TestListAssertions_BeyondDefaultPage(t *testing.T) {
	const tools = 20 // the server's default page size is 15

	ts := mcptest.NewServer(t, manyToolsServer(tools))
	ts.Initialize().AssertOk()

	page := ts.Send("tools/list", map[string]any{})
	page.AssertToolCount(15).AssertToolListed("tool-0").AssertToolNotListed("tool-15")
	if next, _ := page.Result()["nextCursor"].(string); next == "" {
		t.Fatal("expected the first page of 20 tools to carry a nextCursor")
	}

	ts.ListTools().
		AssertToolCount(tools).
		AssertToolListed("tool-0", "tool-14", "tool-15", "tool-19").
		AssertToolNotListed("tool-20")

	// A tool on a later page must not read as unlisted.
	tb := &fakeTB{}
	beyond := mcptest.NewServer(tb, manyToolsServer(tools))
	if !runAssertion(tb, func() { beyond.ListTools().AssertToolNotListed("tool-19") }) {
		t.Fatal("AssertToolNotListed must fail for a tool listed on a later page")
	}
	if msg := tb.lastMessage(); !strings.Contains(msg, `tool "tool-19" was listed but should not have been`) {
		t.Fatalf("failure message %q does not name the tool", msg)
	}
}

// TestListAssertions_ReplyCarriesNoList pins that a registration assertion
// refuses a reply that carries no list at all instead of reading it as an empty
// catalogue. The realistic trigger is a server built without the method set
// installed (the blank import of server/methods): every list method then answers
// MethodNotFound, and a "not listed" or a "count is zero" that held against that
// reply would report every registered primitive as absent while the test went
// green. Chaining a list assertion onto another method's reply is the same
// mistake made by hand.
func TestListAssertions_ReplyCarriesNoList(t *testing.T) {
	replies := []struct {
		name       string
		reply      func(ts *mcptest.Server) *mcptest.Response
		wantErrors string
	}{
		{
			name: "a method the server does not serve",
			reply: func(ts *mcptest.Server) *mcptest.Response {
				return ts.Send("tools/listt", map[string]any{})
			},
			wantErrors: "errors: [The method [tools/listt] was not found.]",
		},
		{
			name: "another method's reply",
			reply: func(ts *mcptest.Server) *mcptest.Response {
				return ts.CallTool("add", map[string]any{"a": 1, "b": 1})
			},
			wantErrors: "errors: (none)",
		},
	}

	// Every one of these holds vacuously against a reply with no list, which is
	// exactly why each must report instead. The tools, prompts, resources and
	// templates below are all registered on assertServer.
	assertions := []struct {
		name    string
		assert  func(r *mcptest.Response)
		wantSub string
	}{
		{"AssertToolNotListed", func(r *mcptest.Response) { r.AssertToolNotListed("add") }, `carry a "tools" list`},
		{"AssertToolListed", func(r *mcptest.Response) { r.AssertToolListed("add") }, `carry a "tools" list`},
		{"AssertToolListed with no names", func(r *mcptest.Response) { r.AssertToolListed() }, `carry a "tools" list`},
		{"AssertToolCount zero", func(r *mcptest.Response) { r.AssertToolCount(0) }, `carry a "tools" list`},
		{"AssertPromptNotListed", func(r *mcptest.Response) { r.AssertPromptNotListed("echo") }, `carry a "prompts" list`},
		{"AssertPromptCount zero", func(r *mcptest.Response) { r.AssertPromptCount(0) }, `carry a "prompts" list`},
		{"AssertResourceNotListed", func(r *mcptest.Response) { r.AssertResourceNotListed("greeting") }, `carry a "resources" list`},
		{"AssertResourceCount zero", func(r *mcptest.Response) { r.AssertResourceCount(0) }, `carry a "resources" list`},
		{
			"AssertResourceTemplateNotListed",
			func(r *mcptest.Response) { r.AssertResourceTemplateNotListed("doc") },
			`carry a "resourceTemplates" list`,
		},
		{
			"AssertResourceTemplateCount zero",
			func(r *mcptest.Response) { r.AssertResourceTemplateCount(0) },
			`carry a "resourceTemplates" list`,
		},
		{"Tool entry lookup", func(r *mcptest.Response) { r.Tool("add") }, `carry a "tools" list`},
		{"Prompt entry lookup", func(r *mcptest.Response) { r.Prompt("echo") }, `carry a "prompts" list`},
	}

	for _, reply := range replies {
		t.Run(reply.name, func(t *testing.T) {
			for _, tt := range assertions {
				t.Run(tt.name, func(t *testing.T) {
					tb := &fakeTB{}
					ts := mcptest.NewServer(tb, assertServer())
					res := reply.reply(ts)
					if !runAssertion(tb, func() { tt.assert(res) }) {
						t.Fatalf("%s must fail against a reply that carries no list", tt.name)
					}
					msg := tb.lastMessage()
					if !strings.Contains(msg, tt.wantSub) {
						t.Fatalf("failure message %q does not name the missing list (%q)", msg, tt.wantSub)
					}
					// The message must also say what the reply did carry, which is
					// how a reader learns the method set is missing.
					if !strings.Contains(msg, reply.wantErrors) {
						t.Fatalf("failure message %q does not report %q", msg, reply.wantErrors)
					}
				})
			}
		})
	}
}

// TestListAssertions_EmptyCatalogue is the other half of the guard above: a
// server that really has nothing registered answers with an empty list, which
// the assertions must read as an empty catalogue rather than reject.
func TestListAssertions_EmptyCatalogue(t *testing.T) {
	ts := mcptest.NewServer(t, server.New("empty", "1.0.0"))
	ts.Initialize().AssertOk()

	ts.ListTools().AssertOk().AssertToolListed().AssertToolNotListed("add").AssertToolCount(0)
	ts.ListPrompts().AssertPromptNotListed("echo").AssertPromptCount(0)
	ts.ListResources().AssertResourceNotListed("greeting").AssertResourceCount(0)
	ts.ListResourceTemplates().AssertResourceTemplateNotListed("doc").AssertResourceTemplateCount(0)
}

func TestListedEntryAssertions(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	tools := ts.ListTools()
	tools.Tool("add").
		AssertName("add").
		AssertTitle("Add").
		AssertDescription("Add two numbers")
	tools.Tool("ship-it").
		AssertName("ship-it").
		AssertTitle("Ship It Now").
		AssertDescription("Ship the build")

	// A primitive that declares no title of its own is advertised with a
	// headline derived from its name, and the assertion reads that.
	ts.ListPrompts().Prompt("search").
		AssertName("search").
		AssertTitle("Search").
		AssertDescription("Search the docs")

	ts.ListResources().Resource("greeting").
		AssertName("greeting").
		AssertTitle("Greeting").
		AssertDescription("A friendly greeting")

	entry := ts.ListResourceTemplates().ResourceTemplate("doc")
	entry.AssertName("doc").AssertTitle("Doc").AssertDescription("A documentation page")
	if got, _ := entry.Raw()["uriTemplate"].(string); got != "file://docs/{slug}" {
		t.Fatalf("resource template uriTemplate = %q, want file://docs/{slug}", got)
	}
}

func TestNotRegisteredAssertions(t *testing.T) {
	// A server with nothing registered: every invocation must come back as the
	// "not found" error the registration assertions look for.
	empty := server.New("empty", "1.0.0", server.WithCapability(server.CapabilityCompletions))
	ts := mcptest.NewServer(t, empty)
	ts.Initialize().AssertOk()

	ts.CallTool("add", nil).AssertToolNotRegistered("add")
	ts.GetPrompt("echo", nil).AssertPromptNotRegistered("echo")
	ts.ReadResource("file://greeting.txt").AssertResourceNotRegistered("file://greeting.txt")
	// A completion reference to a missing primitive reports the same shape.
	ts.CompletePrompt("echo", "topic", "", nil).AssertPromptNotRegistered("echo")
	ts.CompleteResource("file://docs/x", "slug", "", nil).AssertResourceNotRegistered("file://docs/x")
}

// TestNotRegisteredAssertions_HandlerImitatingNotFound separates the two replies
// a registration assertion must tell apart. A tools/call naming a tool the
// server does not have is refused before any handler runs, and the client is
// answered with a protocol error; a registered tool that answers with the same
// text produces a tool error result, which reports that the tool exists and its
// handler failed. Only the first is evidence of an unregistered tool, so the
// negative-path table below drives AssertToolNotRegistered against the second
// and expects it to fail.
func TestNotRegisteredAssertions_HandlerImitatingNotFound(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	// The impostor is registered and advertised, and its misleading text does
	// reach the client, as the content of a tool error result.
	ts.ListTools().AssertToolListed("impostor")
	res := ts.CallTool("impostor", nil).AssertError("Tool [impostor] not found.")
	if raw := res.Raw(); raw == nil || raw.Error != nil {
		t.Fatalf("a registered handler must answer with a result, got %+v", raw)
	}

	// The same server answers a tool it really does not have with the protocol
	// error, which is the shape the assertion is for.
	ghost := ts.CallTool("ghost", nil).AssertToolNotRegistered("ghost")
	ghost.AssertErrorCode(jsonrpc.CodeInvalidParams)
}

func TestStructuredContentAssertions(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	res := ts.CallTool("report", nil)
	res.AssertOk().
		AssertText("2 passed, 1 failed").
		AssertStructuredContent(map[string]any{
			"passed": 2,
			"failed": 1,
			"tags":   []string{"unit", "race"},
		}).
		AssertStructuredContentKey("passed", 2).
		AssertStructuredContentKey("tags", []string{"unit", "race"})

	// The expectation is serialized before comparison, so the Go int 2 above and
	// the float64 the reply decodes to are the same document.
	if got := res.StructuredContent()["passed"]; got != float64(2) {
		t.Fatalf("decoded structuredContent[passed] = %#v, want float64(2)", got)
	}

	// A tool that returns only text advertises no structuredContent.
	ts.CallTool("add", map[string]any{"a": 1, "b": 1}).
		AssertOk().
		AssertNoStructuredContent()
}

// TestStructuredContentAssertions_Serialized states the expectation with the
// typed value a caller has in hand rather than as a map: the assertion
// serializes it first, so the Go float 10 and the wire's 10 are one document.
func TestStructuredContentAssertions_Serialized(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	ts.CallTool("booking", nil).
		AssertOk().
		AssertStructuredContent(booking{Status: "confirmed", Amount: 10}).
		AssertStructuredContentKey("amount", 10).
		AssertStructuredContentKey("status", "confirmed")

	// A struct that differs in one field is reported, so the serialization is
	// not swallowing the comparison.
	tb := &fakeTB{}
	failing := mcptest.NewServer(tb, assertServer())
	if !runAssertion(tb, func() {
		failing.CallTool("booking", nil).AssertStructuredContent(booking{Status: "pending", Amount: 10})
	}) {
		t.Fatal("a struct expectation with a wrong field must fail")
	}
	if msg := tb.lastMessage(); !strings.Contains(msg, `want {"status":"pending","amount":10}`) {
		t.Fatalf("failure message %q does not report the expectation", msg)
	}
}

// TestStructuredContentAssertions_LargeInteger pins that the comparison runs on
// the bytes the server wrote: a record id above 2^53 would compare equal to its
// neighbour if either side were routed through float64.
func TestStructuredContentAssertions_LargeInteger(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	ts.CallTool("ledger", nil).
		AssertOk().
		AssertStructuredContent(map[string]any{"id": int64(9007199254740993)}).
		AssertStructuredContentKey("id", int64(9007199254740993))

	tb := &fakeTB{}
	failing := mcptest.NewServer(tb, assertServer())
	if !runAssertion(tb, func() {
		failing.CallTool("ledger", nil).AssertStructuredContentKey("id", int64(9007199254740992))
	}) {
		t.Fatal("an id differing in the last digit must fail")
	}
	if msg := tb.lastMessage(); !strings.Contains(msg, `structuredContent["id"] = 9007199254740993, want 9007199254740992`) {
		t.Fatalf("failure message %q loses the id precision", msg)
	}
}

// TestStructuredContentAssertions_NumberSpelling pins that the comparison is
// numeric and not textual. A tool handing back an upstream reply's bytes writes
// 10.0 and 1e2 on the wire, and an expectation stated as a Go number has to
// match them; the id beyond float64's exact range must still differ from its
// neighbour, so both properties are asserted against one reply.
func TestStructuredContentAssertions_NumberSpelling(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	ts.CallTool("upstream", nil).
		AssertOk().
		AssertStructuredContentKey("amount", 10).
		AssertStructuredContentKey("amount", 10.0).
		AssertStructuredContentKey("ratio", 100).
		AssertStructuredContentKey("id", int64(9007199254740993)).
		AssertStructuredContent(map[string]any{
			"amount": 10,
			"ratio":  100,
			"id":     int64(9007199254740993),
		})

	negatives := []struct {
		name    string
		assert  func(r *mcptest.Response)
		wantSub string
	}{
		{
			// The failure message reports the spelling the server used, not the
			// normalised form the comparison ran on.
			name:    "a different amount",
			assert:  func(r *mcptest.Response) { r.AssertStructuredContentKey("amount", 11) },
			wantSub: `structuredContent["amount"] = 10.0, want 11`,
		},
		{
			name:    "the neighbouring id",
			assert:  func(r *mcptest.Response) { r.AssertStructuredContentKey("id", int64(9007199254740992)) },
			wantSub: `structuredContent["id"] = 9007199254740993, want 9007199254740992`,
		},
		{
			name:    "a ratio off by one",
			assert:  func(r *mcptest.Response) { r.AssertStructuredContentKey("ratio", 101) },
			wantSub: `structuredContent["ratio"] = 1e2, want 101`,
		},
	}
	for _, tt := range negatives {
		t.Run(tt.name, func(t *testing.T) {
			tb := &fakeTB{}
			failing := mcptest.NewServer(tb, assertServer())
			res := failing.CallTool("upstream", nil)
			if !runAssertion(tb, func() { tt.assert(res) }) {
				t.Fatalf("expected %s to fail", tt.name)
			}
			if msg := tb.lastMessage(); !strings.Contains(msg, tt.wantSub) {
				t.Fatalf("failure message %q does not contain %q", msg, tt.wantSub)
			}
		})
	}
}

func TestNotificationAssertions(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	res := callWithProgressToken(ts, "import", "tok-1")
	res.AssertOk().
		AssertText("imported").
		AssertNotificationCount(2).
		AssertSentNotification("notifications/progress").
		AssertNotSentNotification("notifications/message").
		AssertSentNotificationWith("notifications/progress", map[string]any{
			"progressToken": "tok-1",
			"progress":      1,
			"total":         2,
			"message":       "half",
		}).
		AssertSentNotificationWith("notifications/progress", map[string]any{
			"progressToken": "tok-1",
			"progress":      2,
			"total":         2,
			"message":       "done",
		})

	// A nil params expectation matches on the method alone.
	res.AssertSentNotificationWith("notifications/progress", nil)

	notes := res.SentNotifications()
	if len(notes) != 2 {
		t.Fatalf("SentNotifications len = %d, want 2", len(notes))
	}
	if notes[0].Method != "notifications/progress" {
		t.Fatalf("first notification method = %q, want notifications/progress", notes[0].Method)
	}
	// The caller owns the result outright: the slice, the notifications in it
	// and their params buffers. Each is written into below, and the recording
	// must be unmoved afterwards.
	before := string(notes[0].Params)
	notes[0].Method = "notifications/clobbered"
	for i := range notes[0].Params {
		notes[0].Params[i] = 'x'
	}
	notes[1] = nil

	fresh := res.SentNotifications()
	if fresh[0] == nil {
		t.Fatal("SentNotifications must return a copy of the recording slice")
	}
	if fresh[0].Method != "notifications/progress" {
		t.Fatalf("recording method = %q after a caller retitled its copy, want notifications/progress", fresh[0].Method)
	}
	if got := string(fresh[0].Params); got != before {
		t.Fatalf("recording params = %s after a caller wrote into its copy, want %s", got, before)
	}
	res.AssertNotificationCount(2).
		AssertSentNotification("notifications/progress").
		AssertNotSentNotification("notifications/clobbered").
		AssertSentNotificationWith("notifications/progress", map[string]any{
			"progressToken": "tok-1",
			"progress":      1,
			"total":         2,
			"message":       "half",
		})

	// Without a progressToken the handler's reports are dropped, so the same
	// call emits nothing.
	ts.CallTool("import", nil).
		AssertOk().
		AssertNotificationCount(0).
		AssertNotSentNotification("notifications/progress")

	// Notifications are attributed to the message that produced them, not to
	// every message on the session.
	ts.Ping().AssertOk().AssertNotificationCount(0)
}

func TestNotificationAssertions_AcrossMessages(t *testing.T) {
	// Two successive progress calls on one harness must not pool their frames:
	// each reply sees only the notifications its own message produced.
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	first := callWithProgressToken(ts, "import", "tok-a")
	second := callWithProgressToken(ts, "import", "tok-b")

	first.AssertNotificationCount(2).
		AssertSentNotificationWith("notifications/progress", map[string]any{
			"progressToken": "tok-a", "progress": 1, "total": 2, "message": "half",
		})
	second.AssertNotificationCount(2).
		AssertSentNotificationWith("notifications/progress", map[string]any{
			"progressToken": "tok-b", "progress": 1, "total": 2, "message": "half",
		})
	// The first reply must not have picked up the second call's token.
	second.AssertNotSentNotification("notifications/cancelled")
	for _, note := range first.SentNotifications() {
		if strings.Contains(string(note.Params), "tok-b") {
			t.Fatalf("first reply captured a later message's notification: %s", note.Params)
		}
	}
}

func TestCompletionAssertions(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	ts.CompletePrompt("search", "language", "g", nil).
		AssertOk().
		AssertHasCompletions().
		AssertHasCompletions("go", "gleam").
		AssertCompletionValues("go", "gleam").
		AssertCompletionCount(2).
		AssertCompletionTotal(2).
		AssertCompletionHasMore(false)

	// An empty partial value matches every candidate, in declaration order.
	ts.CompletePrompt("search", "language", "", nil).
		AssertCompletionValues("go", "gleam", "rust").
		AssertCompletionCount(3)

	// An argument the primitive does not complete yields an empty candidate set,
	// not a missing completion object.
	ts.CompletePrompt("search", "unknown-arg", "", nil).
		AssertOk().
		AssertHasCompletions().
		AssertCompletionCount(0).
		AssertCompletionValues()

	// Resource template variables complete the same way. Whether the resolved
	// sibling arguments reach the primitive is pinned by
	// TestCompletionAssertions_Context below, against a primitive that reads
	// them.
	ts.CompleteResource("file://docs/{slug}", "slug", "int", map[string]string{"locale": "en"}).
		AssertOk().
		AssertCompletionValues("intro", "internals").
		AssertCompletionTotal(2)

	if got := ts.CompletePrompt("search", "language", "ru", nil).CompletionValues(); len(got) != 1 || got[0] != "rust" {
		t.Fatalf("CompletionValues = %v, want [rust]", got)
	}
	// A reply that is not a completion carries no values.
	if got := ts.Ping().CompletionValues(); len(got) != 0 {
		t.Fatalf("CompletionValues on a ping reply = %v, want empty", got)
	}
}

// TestCompletionAssertions_Context pins the resolved sibling arguments the
// completion drivers carry: the client passes what it has already resolved, and
// the server hands it to the primitive as the completion context, so a candidate
// set that depends on a sibling argument is the observable proof that the
// context reached it. A driver that dropped the resolved map (or spelled the
// wire key wrong) would answer every one of these with an empty candidate set.
func TestCompletionAssertions_Context(t *testing.T) {
	ts := mcptest.NewServer(t, contextServer())
	ts.Initialize().AssertOk()

	// The argument the others depend on completes on its own.
	ts.CompletePrompt("task", "projectId", "", nil).
		AssertOk().
		AssertCompletionValues("project-1", "project-2").
		AssertCompletionCount(2)

	// With nothing resolved there is nothing to complete: a present completion
	// object carrying no candidates, not a missing one.
	ts.CompletePrompt("task", "taskId", "", nil).
		AssertOk().
		AssertHasCompletions().
		AssertCompletionCount(0).
		AssertCompletionValues()

	// Each resolved value selects its own candidate set, so the context is read
	// rather than merely accepted.
	ts.CompletePrompt("task", "taskId", "", map[string]string{"projectId": "project-1"}).
		AssertOk().
		AssertCompletionValues("task-1-1", "task-1-2").
		AssertCompletionCount(2)
	ts.CompletePrompt("task", "taskId", "", map[string]string{"projectId": "project-2"}).
		AssertOk().
		AssertCompletionValues("task-2-1", "task-2-2")

	// The partial value still filters within the selected set.
	ts.CompletePrompt("task", "taskId", "task-1-2", map[string]string{"projectId": "project-1"}).
		AssertCompletionValues("task-1-2")

	// A resource template variable reads its context the same way.
	const files = "file://users/{userId}/files/{fileId}"
	ts.CompleteResource(files, "fileId", "", nil).
		AssertOk().
		AssertCompletionCount(0)
	ts.CompleteResource(files, "fileId", "", map[string]string{"userId": "user-1"}).
		AssertOk().
		AssertCompletionValues("file1.txt", "file2.txt")
	ts.CompleteResource(files, "fileId", "", map[string]string{"userId": "user-2"}).
		AssertCompletionValues("doc1.txt", "doc2.txt")
}

// TestCompletionAssertions_Truncated drives the one reply in which the count,
// the total and the hasMore flag disagree: more matches than a completion result
// may carry. Each of the three assertions reads a different member of the wire
// completion, so each is pinned against the other two's value.
func TestCompletionAssertions_Truncated(t *testing.T) {
	const total = server.MaxCompletionValues + 5

	ts := mcptest.NewServer(t, bulkServer())
	ts.Initialize().AssertOk()

	ts.CompletePrompt("bulk", "tag", "tag-", nil).
		AssertOk().
		AssertCompletionCount(server.MaxCompletionValues).
		AssertCompletionTotal(total).
		AssertCompletionHasMore(true).
		AssertHasCompletions("tag-000", "tag-099")

	// The values really are the truncated head of the match set: the last five
	// matched candidates did not make it onto the wire.
	values := ts.CompletePrompt("bulk", "tag", "tag-", nil).CompletionValues()
	if len(values) != server.MaxCompletionValues {
		t.Fatalf("completion carried %d values, want %d", len(values), server.MaxCompletionValues)
	}
	if values[len(values)-1] != "tag-099" {
		t.Fatalf("last completion value = %q, want tag-099", values[len(values)-1])
	}

	negatives := []struct {
		name    string
		assert  func(r *mcptest.Response)
		wantSub string
	}{
		{
			name:    "count read as the total",
			assert:  func(r *mcptest.Response) { r.AssertCompletionCount(total) },
			wantSub: fmt.Sprintf("completion carries %d values, want %d", server.MaxCompletionValues, total),
		},
		{
			name:    "total read as the count",
			assert:  func(r *mcptest.Response) { r.AssertCompletionTotal(server.MaxCompletionValues) },
			wantSub: fmt.Sprintf("completion total = %d, want %d", total, server.MaxCompletionValues),
		},
		{
			name:    "hasMore read as false",
			assert:  func(r *mcptest.Response) { r.AssertCompletionHasMore(false) },
			wantSub: "completion hasMore = true, want false",
		},
	}
	for _, tt := range negatives {
		t.Run(tt.name, func(t *testing.T) {
			tb := &fakeTB{}
			failing := mcptest.NewServer(tb, bulkServer())
			failing.Initialize()
			res := failing.CompletePrompt("bulk", "tag", "tag-", nil)
			if !runAssertion(tb, func() { tt.assert(res) }) {
				t.Fatalf("expected %s to fail", tt.name)
			}
			if msg := tb.lastMessage(); !strings.Contains(msg, tt.wantSub) {
				t.Fatalf("failure message %q does not contain %q", msg, tt.wantSub)
			}
		})
	}
}

func TestErrorAssertions(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()

	ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertHasNoErrors()

	// A tool-level error result: the message lives in the content items.
	boom := ts.CallTool("boom", nil)
	boom.AssertHasErrors().AssertHasErrors("kaboom")
	if got := boom.Errors(); len(got) != 1 || got[0] != "kaboom" {
		t.Fatalf("Errors() = %v, want [kaboom]", got)
	}

	// A protocol-level error: the message lives in the JSON-RPC error object.
	missing := ts.CallTool("nope", nil)
	missing.AssertHasErrors("Tool [nope] not found.").AssertErrorCode(jsonrpc.CodeInvalidParams)
	if got := missing.Errors(); len(got) != 1 || got[0] != "Tool [nope] not found." {
		t.Fatalf("Errors() = %v, want the not-found message", got)
	}

	// A clean reply reports an empty, non-nil error set.
	if got := ts.Ping().Errors(); got == nil || len(got) != 0 {
		t.Fatalf("Errors() on a clean reply = %#v, want an empty slice", got)
	}
}

// TestErrorAssertions_MessagelessFailures pins that the error assertions read
// the failure from the result's isError flag and not from the messages they can
// extract. A tool may flag an error result with no content, or with content
// whose text is empty; both are failures the specification states with the flag
// alone, so AssertHasErrors must hold and AssertHasNoErrors must fail, and the
// failure has to say that there was no message rather than print nothing.
func TestErrorAssertions_MessagelessFailures(t *testing.T) {
	tests := []struct {
		name string
		tool string
	}{
		{name: "error result with no content", tool: "silent-failure"},
		{name: "error result with an empty message", tool: "blank-failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := mcptest.NewServer(t, assertServer())
			ts.Initialize().AssertOk()

			// The reply really is an error result on the wire: without this the
			// assertions below would be pinning the wrong thing.
			res := ts.CallTool(tt.tool, nil)
			if isErr, _ := res.Result()["isError"].(bool); !isErr {
				t.Fatalf("%s: result isError = %v, want true", tt.tool, res.Result()["isError"])
			}
			res.AssertHasErrors().AssertError()

			// No message can be extracted, which is exactly why presence may not
			// be read from the message set.
			if got := res.Errors(); len(got) != 0 {
				t.Fatalf("Errors() = %v, want no messages", got)
			}

			for _, assertion := range []struct {
				name string
				run  func(*mcptest.Response)
			}{
				{name: "AssertHasNoErrors", run: func(r *mcptest.Response) { r.AssertHasNoErrors() }},
				{name: "AssertOk", run: func(r *mcptest.Response) { r.AssertOk() }},
			} {
				tb := &fakeTB{}
				failing := mcptest.NewServer(tb, assertServer())
				failing.Initialize()
				failed := failing.CallTool(tt.tool, nil)
				if !runAssertion(tb, func() { assertion.run(failed) }) {
					t.Fatalf("expected %s to fail on a %s reply", assertion.name, tt.tool)
				}
				if msg := tb.lastMessage(); !strings.Contains(msg, "expected no errors, got: (no message)") {
					t.Fatalf("%s failure message %q does not report the missing message", assertion.name, msg)
				}
			}
		})
	}
}

// TestAssertions_NewNegativePaths drives every assertion added here down its
// failure path and pins the text the failure reports, so an assertion that
// stopped checking anything (or reported the wrong thing) fails this test.
func TestAssertions_NewNegativePaths(t *testing.T) {
	tests := []struct {
		name    string
		assert  func(ts *mcptest.Server)
		wantSub string
	}{
		{
			name:    "AssertResourceNotListed present",
			assert:  func(ts *mcptest.Server) { ts.ListResources().AssertResourceNotListed("greeting") },
			wantSub: `resource "greeting" was listed but should not have been`,
		},
		{
			name:    "AssertPromptNotListed present",
			assert:  func(ts *mcptest.Server) { ts.ListPrompts().AssertPromptNotListed("echo") },
			wantSub: `prompt "echo" was listed but should not have been`,
		},
		{
			name:    "AssertResourceCount mismatch",
			assert:  func(ts *mcptest.Server) { ts.ListResources().AssertResourceCount(9) },
			wantSub: "listed 1 resources, want 9",
		},
		{
			name:    "AssertPromptCount mismatch",
			assert:  func(ts *mcptest.Server) { ts.ListPrompts().AssertPromptCount(9) },
			wantSub: "listed 2 prompts, want 9",
		},
		{
			name:    "AssertResourceTemplateListed missing",
			assert:  func(ts *mcptest.Server) { ts.ListResourceTemplates().AssertResourceTemplateListed("ghost") },
			wantSub: `resource template "ghost" was not listed`,
		},
		{
			name:    "AssertResourceTemplateNotListed present",
			assert:  func(ts *mcptest.Server) { ts.ListResourceTemplates().AssertResourceTemplateNotListed("doc") },
			wantSub: `resource template "doc" was listed but should not have been`,
		},
		{
			name:    "AssertResourceTemplateCount mismatch",
			assert:  func(ts *mcptest.Server) { ts.ListResourceTemplates().AssertResourceTemplateCount(4) },
			wantSub: "listed 1 resource templates, want 4",
		},
		// Every name a registration assertion is given is checked, not just the
		// first one: the offending name sits second in each of the pairs below,
		// behind a name that holds. A "not listed" that stopped after the first
		// name would report a registered primitive as absent, which is the worst
		// failure mode this package can have, and a "listed" that stopped would
		// pass for a primitive nobody registered.
		{
			name:    "AssertToolListed missing behind a listed name",
			assert:  func(ts *mcptest.Server) { ts.ListTools().AssertToolListed("add", "ghost") },
			wantSub: `tool "ghost" was not listed`,
		},
		{
			name:    "AssertToolNotListed present behind an absent name",
			assert:  func(ts *mcptest.Server) { ts.ListTools().AssertToolNotListed("ghost", "add") },
			wantSub: `tool "add" was listed but should not have been`,
		},
		{
			name:    "AssertPromptListed missing behind a listed name",
			assert:  func(ts *mcptest.Server) { ts.ListPrompts().AssertPromptListed("echo", "ghost") },
			wantSub: `prompt "ghost" was not listed`,
		},
		{
			name:    "AssertPromptNotListed present behind an absent name",
			assert:  func(ts *mcptest.Server) { ts.ListPrompts().AssertPromptNotListed("ghost", "echo") },
			wantSub: `prompt "echo" was listed but should not have been`,
		},
		{
			name:    "AssertResourceListed missing behind a listed name",
			assert:  func(ts *mcptest.Server) { ts.ListResources().AssertResourceListed("greeting", "ghost") },
			wantSub: `resource "ghost" was not listed`,
		},
		{
			name:    "AssertResourceNotListed present behind an absent name",
			assert:  func(ts *mcptest.Server) { ts.ListResources().AssertResourceNotListed("ghost", "greeting") },
			wantSub: `resource "greeting" was listed but should not have been`,
		},
		{
			name: "AssertResourceTemplateListed missing behind a listed name",
			assert: func(ts *mcptest.Server) {
				ts.ListResourceTemplates().AssertResourceTemplateListed("doc", "ghost")
			},
			wantSub: `resource template "ghost" was not listed`,
		},
		{
			name: "AssertResourceTemplateNotListed present behind an absent name",
			assert: func(ts *mcptest.Server) {
				ts.ListResourceTemplates().AssertResourceTemplateNotListed("ghost", "doc")
			},
			wantSub: `resource template "doc" was listed but should not have been`,
		},
		{
			name:    "Tool entry missing",
			assert:  func(ts *mcptest.Server) { ts.ListTools().Tool("ghost") },
			wantSub: `tool "ghost" was not listed`,
		},
		{
			name:    "Prompt entry missing",
			assert:  func(ts *mcptest.Server) { ts.ListPrompts().Prompt("ghost") },
			wantSub: `prompt "ghost" was not listed`,
		},
		{
			name:    "Resource entry missing",
			assert:  func(ts *mcptest.Server) { ts.ListResources().Resource("ghost") },
			wantSub: `resource "ghost" was not listed`,
		},
		{
			name:    "ResourceTemplate entry missing",
			assert:  func(ts *mcptest.Server) { ts.ListResourceTemplates().ResourceTemplate("ghost") },
			wantSub: `resource template "ghost" was not listed`,
		},
		{
			name:    "AssertName mismatch",
			assert:  func(ts *mcptest.Server) { ts.ListTools().Tool("add").AssertName("plus") },
			wantSub: `tool "add" name = "add", want "plus"`,
		},
		{
			name:    "AssertTitle mismatch",
			assert:  func(ts *mcptest.Server) { ts.ListTools().Tool("ship-it").AssertTitle("Nope") },
			wantSub: `title = "Ship It Now", want "Nope"`,
		},
		{
			name:    "AssertDescription mismatch",
			assert:  func(ts *mcptest.Server) { ts.ListTools().Tool("add").AssertDescription("Nope") },
			wantSub: `description = "Add two numbers", want "Nope"`,
		},
		{
			name: "AssertToolNotRegistered but registered",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertToolNotRegistered("add")
			},
			wantSub: "expected the tool to be unregistered",
		},
		{
			name: "AssertToolNotRegistered other error",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("boom", nil).AssertToolNotRegistered("boom")
			},
			wantSub: `but the reply carries no protocol error; errors: [kaboom]`,
		},
		{
			// A tool error result says the handler ran, so the tool exists,
			// whatever its text claims. Accepting it would report a registered
			// primitive as absent.
			name: "AssertToolNotRegistered against a handler imitating not found",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("impostor", nil).AssertToolNotRegistered("impostor")
			},
			wantSub: `but the reply carries no protocol error; errors: [Tool [impostor] not found.]`,
		},
		{
			name:    "AssertPromptNotRegistered but registered",
			assert:  func(ts *mcptest.Server) { ts.GetPrompt("echo", nil).AssertPromptNotRegistered("echo") },
			wantSub: "expected the prompt to be unregistered",
		},
		{
			name: "AssertResourceNotRegistered but registered",
			assert: func(ts *mcptest.Server) {
				ts.ReadResource("file://greeting.txt").AssertResourceNotRegistered("file://greeting.txt")
			},
			wantSub: "expected the resource to be unregistered",
		},
		{
			// The kind is part of the claim: a missing tool is not evidence that
			// a prompt of that name is unregistered.
			name: "AssertPromptNotRegistered against a tools/call reply",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("ghost", nil).AssertPromptNotRegistered("ghost")
			},
			wantSub: `expected the prompt to be unregistered (error "Prompt [ghost] not found."), got: Tool [ghost] not found.`,
		},
		{
			name: "AssertToolNotRegistered against a prompts/get reply",
			assert: func(ts *mcptest.Server) {
				ts.GetPrompt("ghost", nil).AssertToolNotRegistered("ghost")
			},
			wantSub: `expected the tool to be unregistered (error "Tool [ghost] not found."), got: Prompt [ghost] not found.`,
		},
		{
			name: "AssertResourceNotRegistered against a tools/call reply",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("ghost", nil).AssertResourceNotRegistered("file://ghost.txt")
			},
			wantSub: `expected the resource to be unregistered (error "Resource [file://ghost.txt] not found."), got: Tool [ghost] not found.`,
		},
		{
			name: "AssertStructuredContent mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("report", nil).AssertStructuredContent(map[string]any{"passed": 99})
			},
			wantSub: `structured content = {"failed":1,"passed":2,"tags":["unit","race"]}, want {"passed":99}`,
		},
		{
			// The expectation is the whole document, not a subset: a reply with
			// more keys than the expectation does not match.
			name: "AssertStructuredContent subset",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("report", nil).AssertStructuredContent(map[string]any{"passed": 2})
			},
			wantSub: `structured content = {"failed":1,"passed":2,"tags":["unit","race"]}, want {"passed":2}`,
		},
		{
			name: "AssertStructuredContent when absent",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("add", map[string]any{"a": 1, "b": 1}).
					AssertStructuredContent(map[string]any{"passed": 1})
			},
			wantSub: "but the reply carries none",
		},
		{
			name: "AssertStructuredContentKey missing key",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("report", nil).AssertStructuredContentKey("skipped", 0)
			},
			wantSub: `structured content is missing key "skipped"`,
		},
		{
			name: "AssertStructuredContentKey wrong value",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("report", nil).AssertStructuredContentKey("passed", 7)
			},
			wantSub: `structuredContent["passed"] = 2, want 7`,
		},
		{
			name: "AssertStructuredContentKey when absent",
			assert: func(ts *mcptest.Server) {
				ts.Ping().AssertStructuredContentKey("passed", 1)
			},
			wantSub: "but the reply carries none",
		},
		{
			name: "AssertNoStructuredContent when present",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("report", nil).AssertNoStructuredContent()
			},
			wantSub: "expected no structured content, got",
		},
		{
			name: "AssertNotificationCount mismatch",
			assert: func(ts *mcptest.Server) {
				callWithProgressToken(ts, "import", "tok").AssertNotificationCount(5)
			},
			wantSub: "emitted 2 notifications, want 5; emitted: notifications/progress, notifications/progress",
		},
		{
			name:    "AssertNotificationCount none emitted",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertNotificationCount(1) },
			wantSub: "emitted 0 notifications, want 1; emitted: (none)",
		},
		{
			name:    "AssertSentNotification missing",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertSentNotification("notifications/progress") },
			wantSub: `expected a "notifications/progress" notification, but none was emitted; emitted: (none)`,
		},
		{
			name: "AssertSentNotificationWith no such method",
			assert: func(ts *mcptest.Server) {
				ts.Ping().AssertSentNotificationWith("notifications/progress", map[string]any{"progress": 1})
			},
			wantSub: `but no "notifications/progress" notification was emitted`,
		},
		{
			name: "AssertSentNotificationWith params mismatch",
			assert: func(ts *mcptest.Server) {
				callWithProgressToken(ts, "import", "tok").
					AssertSentNotificationWith("notifications/progress", map[string]any{"progress": 9})
			},
			wantSub: `with params {"progress":9}, got: {"message":"half"`,
		},
		{
			// Correct as far as it goes, but partial: the params expectation is
			// the whole params object.
			name: "AssertSentNotificationWith partial params",
			assert: func(ts *mcptest.Server) {
				callWithProgressToken(ts, "import", "tok").
					AssertSentNotificationWith("notifications/progress", map[string]any{"progress": 1})
			},
			wantSub: `with params {"progress":1}, got: {"message":"half"`,
		},
		{
			// The method is half the claim: params that match a notification the
			// server did emit are no evidence that it emitted one under the
			// method asked about.
			name: "AssertSentNotificationWith another method's params",
			assert: func(ts *mcptest.Server) {
				callWithProgressToken(ts, "import", "tok").
					AssertSentNotificationWith("notifications/message", map[string]any{
						"progressToken": "tok",
						"progress":      1,
						"total":         2,
						"message":       "half",
					})
			},
			wantSub: `but no "notifications/message" notification was emitted; emitted: notifications/progress, notifications/progress`,
		},
		{
			name: "AssertNotSentNotification present",
			assert: func(ts *mcptest.Server) {
				callWithProgressToken(ts, "import", "tok").AssertNotSentNotification("notifications/progress")
			},
			wantSub: `did not expect a "notifications/progress" notification`,
		},
		{
			name: "AssertHasCompletions missing value",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertHasCompletions("python")
			},
			wantSub: `expected completion value "python", got: [go, gleam]`,
		},
		{
			// As with the registration assertions, every value given is checked
			// and not just the first.
			name: "AssertHasCompletions missing behind a present value",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertHasCompletions("go", "python")
			},
			wantSub: `expected completion value "python", got: [go, gleam]`,
		},
		{
			// A candidate is matched whole: a prefix of one is not a candidate.
			name: "AssertHasCompletions prefix of a candidate",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertHasCompletions("g")
			},
			wantSub: `expected completion value "g", got: [go, gleam]`,
		},
		{
			name: "AssertHasCompletions with no candidates",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "unknown-arg", "", nil).AssertHasCompletions("go")
			},
			wantSub: `expected completion value "go", got: (none)`,
		},
		{
			name:    "AssertHasCompletions on a non-completion reply",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertHasCompletions() },
			wantSub: "expected a completion in the reply, but the reply carries no completion",
		},
		{
			name: "AssertCompletionValues mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertCompletionValues("gleam", "go")
			},
			wantSub: "completion values = [go, gleam], want [gleam, go]",
		},
		{
			name:    "AssertCompletionValues on a non-completion reply",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertCompletionValues("go") },
			wantSub: "the reply carries no completion",
		},
		{
			name: "AssertCompletionCount mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertCompletionCount(3)
			},
			wantSub: "completion carries 2 values, want 3; got: [go, gleam]",
		},
		{
			name:    "AssertCompletionCount on a non-completion reply",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertCompletionCount(0) },
			wantSub: "the reply carries no completion",
		},
		{
			name: "AssertCompletionTotal mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertCompletionTotal(7)
			},
			wantSub: "completion total = 2, want 7",
		},
		{
			name:    "AssertCompletionTotal on a non-completion reply",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertCompletionTotal(0) },
			wantSub: "the reply carries no completion",
		},
		{
			name: "AssertCompletionHasMore mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CompletePrompt("search", "language", "g", nil).AssertCompletionHasMore(true)
			},
			wantSub: "completion hasMore = false, want true",
		},
		{
			name:    "AssertCompletionHasMore on a non-completion reply",
			assert:  func(ts *mcptest.Server) { ts.Ping().AssertCompletionHasMore(false) },
			wantSub: "the reply carries no completion",
		},
		{
			name: "AssertHasNoErrors with an error",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("boom", nil).AssertHasNoErrors()
			},
			wantSub: "expected no errors, got: kaboom",
		},
		{
			name: "AssertHasErrors without one",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("add", map[string]any{"a": 1, "b": 1}).AssertHasErrors()
			},
			wantSub: "expected an error",
		},
		{
			// A harness failure (as opposed to a server behaviour under test) is
			// reported rather than silently driving an empty message.
			name: "Send with params that cannot be serialized",
			assert: func(ts *mcptest.Server) {
				ts.Send("tools/list", map[string]any{"bad": make(chan int)})
			},
			wantSub: `marshal params for "tools/list"`,
		},
		{
			name: "Notify with params that cannot be serialized",
			assert: func(ts *mcptest.Server) {
				ts.Notify("notifications/progress", map[string]any{"bad": make(chan int)})
			},
			wantSub: `marshal params for notification "notifications/progress"`,
		},
		{
			name: "AssertHasErrors message mismatch",
			assert: func(ts *mcptest.Server) {
				ts.CallTool("boom", nil).AssertHasErrors("not-this")
			},
			wantSub: `expected error containing "not-this", got: kaboom`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := &fakeTB{}
			ts := mcptest.NewServer(tb, assertServer())
			if !runAssertion(tb, func() { tt.assert(ts) }) {
				t.Fatalf("expected assertion %q to fail", tt.name)
			}
			if msg := tb.lastMessage(); !strings.Contains(msg, tt.wantSub) {
				t.Fatalf("failure message %q does not contain %q", msg, tt.wantSub)
			}
		})
	}
}

// TestAssertions_NilT_NewAssertions checks that every assertion added here
// degrades to a no-op (rather than panicking) when the harness carries no
// testing.TB, including on the paths that would otherwise fail.
func TestAssertions_NilT_NewAssertions(t *testing.T) {
	ts := mcptest.NewServer(nil, assertServer())
	ts.Initialize()

	ts.ListResources().AssertResourceNotListed("greeting").AssertResourceCount(99)
	ts.ListPrompts().AssertPromptNotListed("echo").AssertPromptCount(99)
	ts.ListResourceTemplates().
		AssertResourceTemplateListed("ghost").
		AssertResourceTemplateNotListed("doc").
		AssertResourceTemplateCount(99)

	// A missing entry yields a Listed with no item; its assertions must not
	// dereference it.
	ghost := ts.ListTools().Tool("ghost")
	ghost.AssertName("ghost").AssertTitle("Ghost").AssertDescription("boo")
	if ghost.Raw() != nil {
		t.Fatal("a missing entry must expose a nil Raw()")
	}
	ts.ListPrompts().Prompt("ghost").AssertName("x")
	ts.ListResources().Resource("ghost").AssertName("x")
	ts.ListResourceTemplates().ResourceTemplate("ghost").AssertName("x")

	ts.CallTool("add", map[string]any{"a": 1, "b": 1}).
		AssertToolNotRegistered("add").
		AssertStructuredContent(map[string]any{"a": 1}).
		AssertStructuredContentKey("a", 1).
		AssertNotificationCount(3).
		AssertSentNotification("notifications/progress").
		AssertSentNotificationWith("notifications/progress", map[string]any{"a": 1}).
		AssertHasErrors("nope")
	ts.GetPrompt("echo", nil).AssertPromptNotRegistered("echo")
	ts.ReadResource("file://greeting.txt").AssertResourceNotRegistered("file://greeting.txt")
	ts.CallTool("report", nil).
		AssertNoStructuredContent().
		AssertStructuredContentKey("missing", 1)
	callWithProgressToken(ts, "import", "tok").
		AssertNotSentNotification("notifications/progress").
		AssertSentNotificationWith("notifications/progress", map[string]any{"progress": 99})
	ts.Ping().
		AssertHasCompletions("go").
		AssertCompletionValues("go").
		AssertCompletionCount(1).
		AssertCompletionTotal(1).
		AssertCompletionHasMore(true).
		AssertHasNoErrors()
}

// TestResponseAssertions_Concurrent checks that the assertions are safe to run
// concurrently against one reply: a Response is immutable once the driven
// message has been handled, and the slices the accessors hand out belong to the
// caller (SentNotifications copies the recording down to each notification and
// its params bytes; Errors builds its result from the reply on each call). Both
// are written into below, on eight goroutines at once, down to the notification
// methods and params buffers, so an accessor handing out shared state would be
// reported by -race and would leave the assertions after the wait reading the
// clobbered values. The
// decoded maps (Result, StructuredContent, Listed.Raw) are shared by reference
// and documented as read-only, so this test reads them without writing.
func TestResponseAssertions_Concurrent(t *testing.T) {
	ts := mcptest.NewServer(t, assertServer())
	ts.Initialize().AssertOk()
	res := callWithProgressToken(ts, "import", "tok-1")
	boom := ts.CallTool("boom", nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.AssertNotificationCount(2).
				AssertSentNotification("notifications/progress").
				AssertHasNoErrors()
			notes := res.SentNotifications()
			for _, note := range notes {
				note.Method = "notifications/clobbered"
				for i := range note.Params {
					note.Params[i] = 'x'
				}
			}
			if len(notes) > 0 {
				notes[0] = nil
			}
			errs := boom.Errors()
			if len(errs) > 0 {
				errs[0] = "clobbered"
			}
			_ = res.StructuredContent()
		}()
	}
	wg.Wait()

	res.AssertNotificationCount(2).
		AssertSentNotification("notifications/progress").
		AssertNotSentNotification("notifications/clobbered").
		AssertSentNotificationWith("notifications/progress", map[string]any{
			"progressToken": "tok-1", "progress": 1, "total": 2, "message": "half",
		})
	boom.AssertHasErrors("kaboom")
	if got := boom.Errors(); len(got) != 1 || got[0] != "kaboom" {
		t.Fatalf("Errors() = %v after concurrent callers wrote into their own copies, want [kaboom]", got)
	}
}
