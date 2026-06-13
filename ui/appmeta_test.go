package ui

import (
	"reflect"
	"strings"
	"testing"
)

func TestAppMetaToMapDefaults(t *testing.T) {
	m := NewAppMeta().ToMap()
	// NewAppMeta seeds prefersBorder=true and nothing else.
	if m["prefersBorder"] != true {
		t.Fatalf("prefersBorder = %v, want true", m["prefersBorder"])
	}
	if _, ok := m["csp"]; ok {
		t.Fatalf("csp should be absent by default: %v", m)
	}
}

func TestAppMetaFull(t *testing.T) {
	meta := NewAppMeta().
		WithDomain("example.com").
		WithPrefersBorder(false).
		WithPermissions(NewPermissions().Allow(PermissionCamera, PermissionClipboardWrite)).
		WithCsp(NewCsp().WithConnectDomains("https://api.example.com")).
		WithLibraries(LibraryTailwind)

	m := meta.ToMap()
	if m["domain"] != "example.com" || m["prefersBorder"] != false {
		t.Fatalf("scalar fields = %v", m)
	}

	perms := m["permissions"].(map[string]any)
	if _, ok := perms["camera"]; !ok {
		t.Fatalf("camera permission missing: %v", perms)
	}
	if _, ok := perms["clipboardWrite"]; !ok {
		t.Fatalf("clipboardWrite permission missing: %v", perms)
	}

	csp := m["csp"].(map[string]any)
	if !reflect.DeepEqual(csp["connectDomains"], []string{"https://api.example.com"}) {
		t.Fatalf("connectDomains = %v", csp["connectDomains"])
	}
	// Tailwind's CDN host is merged into resourceDomains automatically.
	rd, _ := csp["resourceDomains"].([]string)
	if len(rd) != 1 || rd[0] != "https://cdn.tailwindcss.com" {
		t.Fatalf("library domain not merged into resourceDomains: %v", csp["resourceDomains"])
	}
}

func TestAppMetaScriptTags(t *testing.T) {
	tags := NewAppMeta().WithLibraries(LibraryTailwind, LibraryAlpine).ScriptTags()
	if !strings.Contains(tags, "cdn.tailwindcss.com") || !strings.Contains(tags, "alpinejs") {
		t.Fatalf("script tags missing libraries: %q", tags)
	}
	if NewAppMeta().ScriptTags() != "" {
		t.Fatal("no libraries should yield empty script tags")
	}
}

func TestCspMergeDedup(t *testing.T) {
	// An explicit resource domain equal to a library domain is not duplicated.
	meta := NewAppMeta().
		WithCsp(NewCsp().WithResourceDomains("https://cdn.tailwindcss.com")).
		WithLibraries(LibraryTailwind)
	csp := meta.ToMap()["csp"].(map[string]any)
	rd := csp["resourceDomains"].([]string)
	if len(rd) != 1 {
		t.Fatalf("expected dedup to 1 domain, got %v", rd)
	}
}

func TestPermissionsDedupAndEmpty(t *testing.T) {
	if !NewPermissions().isEmpty() {
		t.Fatal("new permissions should be empty")
	}
	p := NewPermissions().Allow(PermissionCamera).Allow(PermissionCamera)
	if len(p.ToMap()) != 1 {
		t.Fatalf("duplicate permission not deduped: %v", p.ToMap())
	}
}
