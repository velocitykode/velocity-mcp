package server

import (
	"context"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
)

func TestToolBuilder(t *testing.T) {
	var schemaCalled bool
	tool := NewTool("add", "adds numbers").
		WithTitle("Adder").
		WithSchema(func(s *schema.Object) {
			schemaCalled = true
			s.Number("a").Required()
		}).
		HandleFunc(func(ctx context.Context, req *Request) (*Response, error) {
			return Text("ok"), nil
		})

	if tool.Name() != "add" || tool.Description() != "adds numbers" || tool.Title() != "Adder" {
		t.Fatalf("metadata = %q/%q/%q", tool.Name(), tool.Description(), tool.Title())
	}

	obj := schema.NewObject()
	tool.Schema(obj)
	if !schemaCalled {
		t.Fatal("schema callback not invoked")
	}

	resp, err := tool.Handle(context.Background(), NewRequest(nil))
	if err != nil || resp.Contents()[0].String() != "ok" {
		t.Fatalf("handle = %v / %v", resp, err)
	}
}

func TestToolBuilderNoHandler(t *testing.T) {
	tool := NewTool("broken", "no handler")
	resp, err := tool.Handle(context.Background(), NewRequest(nil))
	if err != nil {
		t.Fatalf("a handler-less tool should not error: %v", err)
	}
	if !resp.IsError() {
		t.Fatal("a handler-less tool should return an error result")
	}
}

func TestToolBuilderNoSchema(t *testing.T) {
	// Calling Schema with no callback configured must not panic.
	NewTool("t", "d").Schema(schema.NewObject())
}

func TestToolBuilderAnnotationsUnset(t *testing.T) {
	// A tool with no hints configured reports an empty annotations object.
	tool := NewTool("t", "d")
	if m := tool.Annotations().ToMap(); len(m) != 0 {
		t.Fatalf("unset annotations = %v, want empty", m)
	}
}

func TestToolBuilderAnnotations(t *testing.T) {
	// Each hint maps to its MCP wire key, and an explicit false survives (not
	// dropped) because the fields are pointers.
	tool := NewTool("wipe", "deletes things").
		WithReadOnlyHint(false).
		WithDestructiveHint(true).
		WithIdempotentHint(false).
		WithOpenWorldHint(true)

	want := map[string]any{
		"readOnlyHint":    false,
		"destructiveHint": true,
		"idempotentHint":  false,
		"openWorldHint":   true,
	}
	got := tool.Annotations().ToMap()
	if len(got) != len(want) {
		t.Fatalf("annotations = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("annotations[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestToolBuilderAnnotationsPartial(t *testing.T) {
	// Only the configured hint appears; unset hints are omitted entirely.
	tool := NewTool("list", "reads things").WithReadOnlyHint(true)
	got := tool.Annotations().ToMap()
	if len(got) != 1 || got["readOnlyHint"] != true {
		t.Fatalf("partial annotations = %v, want only readOnlyHint:true", got)
	}
}
