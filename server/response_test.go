package server

import (
	"testing"

	"github.com/velocitykode/velocity-mcp/content"
)

func TestText(t *testing.T) {
	r := Text("hi")
	if r.IsError() {
		t.Fatal("Text should not be an error")
	}
	if len(r.Contents()) != 1 {
		t.Fatalf("want 1 content item, got %d", len(r.Contents()))
	}
	if r.Role() != RoleUser {
		t.Fatalf("default role = %q", r.Role())
	}
}

func TestError(t *testing.T) {
	r := Error("bad")
	if !r.IsError() {
		t.Fatal("Error should be an error result")
	}
	if r.Contents()[0].String() != "bad" {
		t.Fatalf("error text = %q", r.Contents()[0].String())
	}
}

func TestNewResponseAndModifiers(t *testing.T) {
	r := NewResponse(content.NewText("a"), content.NewText("b")).
		AsAssistant().
		WithMeta("k", "v").
		WithStructuredContent(map[string]any{"x": 1})

	if r.Role() != RoleAssistant {
		t.Fatalf("role = %q", r.Role())
	}
	if len(r.Contents()) != 2 {
		t.Fatalf("contents = %d", len(r.Contents()))
	}
	if r.Meta()["k"] != "v" {
		t.Fatalf("meta = %v", r.Meta())
	}
	if r.StructuredContent()["x"] != 1 {
		t.Fatalf("structured = %v", r.StructuredContent())
	}
}

func TestImageResponse(t *testing.T) {
	r := Image([]byte{0x1, 0x2}, "image/jpeg")
	if r.IsError() || len(r.Contents()) != 1 {
		t.Fatalf("image response = %+v", r)
	}
	m, err := r.Contents()[0].ToTool()
	if err != nil {
		t.Fatalf("to tool: %v", err)
	}
	if m["type"] != "image" || m["mimeType"] != "image/jpeg" {
		t.Fatalf("image content = %v", m)
	}
}

func TestAudioResponse(t *testing.T) {
	r := Audio([]byte{0x1}, "")
	m, _ := r.Contents()[0].ToTool()
	if m["type"] != "audio" || m["mimeType"] != content.DefaultAudioMimeType {
		t.Fatalf("audio content = %v", m)
	}
}

func TestJSONResponse(t *testing.T) {
	r, err := JSON(map[string]any{"ok": true, "n": 3})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	// Object encodes to both a text item and structuredContent.
	if len(r.Contents()) != 1 {
		t.Fatalf("contents = %d", len(r.Contents()))
	}
	if r.StructuredContent()["ok"] != true {
		t.Fatalf("structured = %v", r.StructuredContent())
	}

	// A non-object (array) yields text only, no structuredContent.
	r2, err := JSON([]int{1, 2})
	if err != nil {
		t.Fatalf("json array: %v", err)
	}
	if r2.StructuredContent() != nil {
		t.Fatalf("array should not set structuredContent: %v", r2.StructuredContent())
	}
	if r2.Contents()[0].String() != "[1,2]" {
		t.Fatalf("array text = %q", r2.Contents()[0].String())
	}

	// An unencodable value returns an error.
	if _, err := JSON(make(chan int)); err == nil {
		t.Fatal("expected error encoding a channel")
	}
}

func TestResponseAsError(t *testing.T) {
	r := Text("oops").AsError()
	if !r.IsError() {
		t.Fatal("AsError should set the error flag")
	}
}

func TestResponseNilSafe(t *testing.T) {
	var r *Response
	if r.IsError() || r.Role() != RoleUser || r.Contents() != nil || r.Meta() != nil || r.StructuredContent() != nil {
		t.Fatal("nil response accessors should be safe and zero-valued")
	}
}

func TestResponseMergeMeta(t *testing.T) {
	r := Text("x").WithMeta("a", 1).WithStructuredContent(map[string]any{"s": true})
	base := map[string]any{"content": []any{}}
	out := r.mergeMeta(base)
	if _, ok := out["_meta"]; !ok {
		t.Fatal("expected _meta merged")
	}
	if _, ok := out["structuredContent"]; !ok {
		t.Fatal("expected structuredContent merged")
	}

	// An existing key is not overwritten.
	base2 := map[string]any{"_meta": "keep"}
	out2 := r.mergeMeta(base2)
	if out2["_meta"] != "keep" {
		t.Fatal("mergeMeta should not overwrite an existing key")
	}
}
