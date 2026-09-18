package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity/validation"

	"github.com/velocitykode/velocity-mcp/schema"
	"github.com/velocitykode/velocity-mcp/server"
)

// uncheckableTool validates a rule field the validation engine cannot address:
// the engine walks nested objects, so it never reaches the element of an array
// the accessors read. The handler reports what it read, so a call that reaches
// the client with a result rather than a failure is visible on the wire.
func uncheckableTool() server.Tool {
	return server.NewTool("first-item", "Reads the first item").
		WithSchema(func(s *schema.Object) {
			s.Array("items").Required()
		}).
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			if err := req.Validate(validation.Rules{
				"items.0": {validation.Nullable(), validation.String(), validation.Max(5)},
			}); err != nil {
				return nil, err
			}
			return server.Text(req.String("items.0")), nil
		})
}

// A tool whose rules cannot check what its handler reads must fail the call.
// The client is told only that the request failed: a refusal is the server's
// own rule at fault, so the response carries the internal error code defined by
// JSON-RPC 2.0 section 5.1 and a message that names neither the rule field, nor
// the reason, nor the argument the handler would have read.
func TestCallToolRefusesRulesThatCannotReachTheField(t *testing.T) {
	// Recognisable in the response body, and far past the rule it would have
	// been checked against.
	const unchecked = "UNCHECKED-VALUE-0123456789"

	s := server.New("demo", "1.0.0", server.WithTools(uncheckableTool()))
	res := handle(t, s, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"first-item","arguments":{"items":["`+unchecked+`"]}}}`)

	if !res.HasResponse || res.Response == nil {
		t.Fatal("tools/call produced no response")
	}
	if res.Response.Error == nil {
		t.Fatalf("tools/call returned a result for a call whose rules checked nothing: %s", res.Response.Result)
	}
	if got := res.Response.Error.Code; got != -32603 {
		t.Fatalf("error code = %d, want -32603 (internal error)", got)
	}
	if got := res.Response.Error.Message; got != "Something went wrong while processing the request." {
		t.Fatalf("error message = %q, want the generic internal error wording", got)
	}
	if res.Response.Error.Data != nil {
		t.Fatalf("error data = %#v, want nothing attached to an internal failure", res.Response.Error.Data)
	}

	// The whole frame is checked, not just the message: a detail attached
	// anywhere in the response would reach the client just the same.
	body, err := json.Marshal(res.Response)
	if err != nil {
		t.Fatalf("encode the response: %v", err)
	}
	for _, leak := range []string{"items.0", unchecked, "addressable", "validation", "rule", "accessor", "mcp:"} {
		if strings.Contains(strings.ToLower(string(body)), strings.ToLower(leak)) {
			t.Fatalf("response leaks %q: %s", leak, body)
		}
	}
}

// The same call under a rule set the engine can address runs and answers with
// the validated value, so the refusal above is the rule field being unreachable
// and not the tool failing for any input.
func TestCallToolRunsWhenTheRulesReachTheField(t *testing.T) {
	tool := server.NewTool("first-item", "Reads the first item").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			if err := req.Validate(validation.Rules{
				"items.first": {validation.Required(), validation.String(), validation.Max(5)},
			}); err != nil {
				return nil, err
			}
			return server.Text(req.String("items.first")), nil
		})

	s := server.New("demo", "1.0.0", server.WithTools(tool))
	res := handle(t, s, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"first-item","arguments":{"items":{"first":"ok"}}}}`)

	result := decodeResult(t, res.Response)
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false", result["isError"])
	}
	item := result["content"].([]any)[0].(map[string]any)
	if item["text"] != "ok" {
		t.Fatalf("text = %#v, want the validated value", item["text"])
	}
}
