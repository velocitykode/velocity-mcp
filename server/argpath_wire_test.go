package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/server"
)

// emptyMemberTool reads the member "" of every item, and of the first item by
// index, and reports both. RFC 8259 section 4 lets an object carry the empty
// string as a member name, so "items.*." and "items.0." each end in a segment
// naming that member. The handler reports exactly what it read, so an accessor
// that answers with more than the member shows on the wire.
func emptyMemberTool() server.Tool {
	return server.NewTool("empty-members", "Reads the empty-named member of every item").
		HandleFunc(func(_ context.Context, req *server.Request) (*server.Response, error) {
			return server.JSON(map[string]any{
				"collected": req.Get("items.*."),
				"first":     req.Get("items.0."),
			})
		})
}

// A separator after a "*" segment is followed by one more segment, the empty
// one, and a tool that reads it gets that member of every element and nothing
// else. Collecting the elements whole instead would put every other member the
// peer sent into a result the handler built from one member.
func TestCallToolCollectsTheEmptyMemberAfterAWildcard(t *testing.T) {
	// Recognisable in the response body wherever it ends up.
	const beside = "BESIDE-THE-MEMBER-0123456789"

	cases := []struct {
		name      string
		arguments string
		// wantText is the exact text content of the result: the two reads the
		// handler made, encoded with sorted keys.
		wantText string
	}{
		{
			name:      "the member of every element",
			arguments: `{"items":[{"":7,"secret":"` + beside + `"},{"":"eight","secret":"` + beside + `"}]}`,
			wantText:  `{"collected":[7,"eight"],"first":7}`,
		},
		{
			name:      "an element without the member leaves a hole",
			arguments: `{"items":[{"secret":"` + beside + `"},{"":false}]}`,
			wantText:  `{"collected":[null,false],"first":null}`,
		},
		{
			name:      "a member that is present and null",
			arguments: `{"items":[{"":null,"secret":"` + beside + `"}]}`,
			wantText:  `{"collected":[null],"first":null}`,
		},
		{
			name:      "scalar elements carry no member",
			arguments: `{"items":["` + beside + `",5,null]}`,
			wantText:  `{"collected":[null,null,null],"first":null}`,
		},
		{
			name:      "the members of an object in key order",
			arguments: `{"items":{"b":{"":"two","secret":"` + beside + `"},"a":{"":"one"}}}`,
			wantText:  `{"collected":["one","two"],"first":null}`,
		},
		{
			name:      "an empty list",
			arguments: `{"items":[]}`,
			wantText:  `{"collected":[],"first":null}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", server.WithTools(emptyMemberTool()))
			res := handle(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"empty-members","arguments":`+tc.arguments+`}}`)

			result := decodeResult(t, res.Response)
			if result["isError"] != false {
				t.Fatalf("isError = %v, want false", result["isError"])
			}
			items, ok := result["content"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("content = %#v, want one text item", result["content"])
			}
			item, _ := items[0].(map[string]any)
			if item["type"] != "text" || item["text"] != tc.wantText {
				t.Fatalf("content item = %#v, want the text %s", item, tc.wantText)
			}

			// The structured form is the same two reads, compared as decoded
			// JSON so the assertion does not depend on member order.
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.wantText), &want); err != nil {
				t.Fatalf("decode the expected reads: %v", err)
			}
			structured, err := json.Marshal(result["structuredContent"])
			if err != nil {
				t.Fatalf("encode structuredContent: %v", err)
			}
			wantStructured, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("encode the expected reads: %v", err)
			}
			if string(structured) != string(wantStructured) {
				t.Fatalf("structuredContent = %s, want %s", structured, wantStructured)
			}

			// The whole frame is checked: what sat beside the member must not
			// reach the client in any part of the response.
			if body := string(res.Response.Result); strings.Contains(body, beside) {
				t.Fatalf("response carries a member the handler never read: %s", body)
			}
		})
	}
}
