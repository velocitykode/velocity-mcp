package server

import (
	"context"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/ui"
)

func TestAppResourceBuilderBasics(t *testing.T) {
	a := NewAppResource("dashboard", "ui://dashboard").
		WithDescription("a dashboard").
		WithAppMeta(ui.NewAppMeta().WithDomain("example.com"))

	if a.Name() != "dashboard" || a.URI() != "ui://dashboard" {
		t.Fatalf("identity = %q %q", a.Name(), a.URI())
	}
	if a.MimeType() != AppResourceMimeType {
		t.Fatalf("mimeType = %q, want %q", a.MimeType(), AppResourceMimeType)
	}
	if a.AppMeta().ToMap()["domain"] != "example.com" {
		t.Fatalf("appMeta domain not carried: %v", a.AppMeta().ToMap())
	}

	// *AppResourceBuilder is both a Resource and an AppResource.
	var _ Resource = a
	var _ AppResource = a
}

func TestAppResourceInjectsLibraryScripts(t *testing.T) {
	a := NewAppResource("dashboard", "ui://dashboard").
		WithAppMeta(ui.NewAppMeta().WithLibraries(ui.LibraryTailwind)).
		HTMLFunc(func(ctx context.Context, req *Request) (string, error) {
			return "<h1>Hi</h1>", nil
		})

	resp, err := a.Read(context.Background(), NewRequest(nil))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	html := resp.Contents()[0].String()
	if !strings.Contains(html, "cdn.tailwindcss.com") {
		t.Fatalf("library script not injected: %q", html)
	}
	if !strings.Contains(html, "<h1>Hi</h1>") {
		t.Fatalf("body missing: %q", html)
	}
	// Scripts precede the body.
	if strings.Index(html, "tailwindcss") > strings.Index(html, "<h1>") {
		t.Fatalf("scripts should precede body: %q", html)
	}
}

func TestAppResourceNoHandler(t *testing.T) {
	a := NewAppResource("x", "ui://x")
	resp, err := a.Read(context.Background(), NewRequest(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError() {
		t.Fatal("a builder with no HTMLFunc should return an error result")
	}
}

func TestAppResourceNoLibrariesNoInjection(t *testing.T) {
	a := NewAppResource("x", "ui://x").
		HTMLFunc(func(ctx context.Context, req *Request) (string, error) { return "<p>x</p>", nil })
	resp, _ := a.Read(context.Background(), NewRequest(nil))
	if resp.Contents()[0].String() != "<p>x</p>" {
		t.Fatalf("body should be untouched without libraries: %q", resp.Contents()[0].String())
	}
}
