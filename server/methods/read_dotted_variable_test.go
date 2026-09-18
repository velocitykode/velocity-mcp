package methods

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/velocitykode/velocity-mcp/server"
)

// dottedTemplate declares a resource template whose variable name carries a
// dot, which RFC 6570 section 2.3 allows in a varname. It reports what each way
// of reading that variable answers, so the read below can assert them from the
// wire.
type dottedTemplate struct{}

func (dottedTemplate) Name() string        { return "user" }
func (dottedTemplate) Description() string { return "a user" }
func (dottedTemplate) URI() string         { return "users://{user.id}" }
func (dottedTemplate) URITemplate() string { return "users://{user.id}" }
func (dottedTemplate) MimeType() string    { return "application/json" }

func (dottedTemplate) Read(_ context.Context, req *server.Request) (*server.Response, error) {
	byName, found := req.Arg("user.id")
	report := map[string]any{
		"arg":   byName,
		"found": found,
		"all":   req.All()["user.id"],
		"path":  req.String("user.id"),
		"has":   req.Has("user.id"),
	}
	b, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return server.Text(string(b)), nil
}

// A template variable whose name contains a dot becomes an argument under that
// exact name, and the resource handler reads it under the name its template
// declared. A variable the uri bound is read under that name alone, so no
// argument the caller sent can answer in its place: the server resolved
// "users://42" and the result names that uri, so the handler must read 42
// whatever the caller spelled alongside it. Arg reads the same variable, and
// resolves no path at all.
func TestReadResourceDottedTemplateVariable(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "no arguments"},
		{
			name: "a nested object spelling the variable path",
			args: map[string]any{"user": map[string]any{"id": "victim"}},
		},
		{
			name: "an argument of the variable's own name",
			args: map[string]any{"user.id": "victim"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ctxWith(server.WithResources(dottedTemplate{}))

			params := map[string]any{"uri": "users://42"}
			if tt.args != nil {
				params["arguments"] = tt.args
			}
			resp, err := ReadResource{}.Handle(c, req(t, 1, "resources/read", params))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			item := decodeResult(t, resp)["contents"].([]any)[0].(map[string]any)
			if item["uri"] != "users://42" {
				t.Fatalf("contents uri = %#v, want the uri the read resolved", item["uri"])
			}

			var report map[string]any
			if err := json.Unmarshal([]byte(item["text"].(string)), &report); err != nil {
				t.Fatalf("decode the handler report: %v", err)
			}

			if report["arg"] != "42" || report["found"] != true {
				t.Fatalf("Arg(user.id) = (%#v,%#v), want (\"42\",true): the variable is an argument of that name", report["arg"], report["found"])
			}
			if report["all"] != "42" {
				t.Fatalf("All()[user.id] = %#v, want the variable as the template declared it", report["all"])
			}
			if report["path"] != "42" {
				t.Fatalf("String(user.id) = %#v, want \"42\": the uri binds the variable, the caller cannot", report["path"])
			}
			if report["has"] != true {
				t.Fatalf("Has(user.id) = %#v, want true: it must agree with the getters", report["has"])
			}
		})
	}
}
