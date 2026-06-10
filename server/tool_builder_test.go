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
