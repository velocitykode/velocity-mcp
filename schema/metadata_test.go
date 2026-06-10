package schema_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
)

func TestIconTheme_Valid(t *testing.T) {
	tests := []struct {
		theme schema.IconTheme
		want  bool
	}{
		{schema.IconThemeLight, true},
		{schema.IconThemeDark, true},
		{schema.IconTheme("sepia"), false},
		{schema.IconTheme(""), false},
	}
	for _, tc := range tests {
		t.Run(string(tc.theme), func(t *testing.T) {
			if got := tc.theme.Valid(); got != tc.want {
				t.Fatalf("Valid() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIcon_ToMap(t *testing.T) {
	tests := []struct {
		name string
		icon schema.Icon
		want map[string]any
	}{
		{
			name: "src-only",
			icon: schema.NewIcon("https://example.com/i.png"),
			want: map[string]any{"src": "https://example.com/i.png"},
		},
		{
			name: "all-fields",
			icon: schema.NewIcon("/icon.svg").
				WithMimeType("image/svg+xml").
				WithSizes("48x48", "96x96").
				WithTheme(schema.IconThemeDark),
			want: map[string]any{
				"src":      "/icon.svg",
				"mimeType": "image/svg+xml",
				"sizes":    []string{"48x48", "96x96"},
				"theme":    "dark",
			},
		},
		{
			name: "empty-sizes-omitted",
			icon: schema.Icon{Src: "x", Sizes: []string{}},
			want: map[string]any{"src": "x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.icon.ToMap()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestIcon_MarshalJSON_OmitsEmpties(t *testing.T) {
	icon := schema.NewIcon("x")
	b, err := json.Marshal(icon)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"src":"x"}` {
		t.Fatalf("got %s", b)
	}
}

func TestIcon_WithHelpersDoNotMutateOriginal(t *testing.T) {
	base := schema.NewIcon("x")
	_ = base.WithMimeType("image/png").WithSizes("1x1").WithTheme(schema.IconThemeLight)
	if base.MimeType != "" || base.Sizes != nil || base.Theme != "" {
		t.Fatalf("With* mutated original: %#v", base)
	}
}

func TestImplementation_ToMap(t *testing.T) {
	tests := []struct {
		name string
		impl schema.Implementation
		want map[string]any
	}{
		{
			name: "name-version-only",
			impl: schema.NewImplementation("demo", "1.0.0"),
			want: map[string]any{"name": "demo", "version": "1.0.0"},
		},
		{
			name: "all-fields",
			impl: schema.NewImplementation("demo", "1.0.0").
				WithTitle("Demo").
				WithDescription("A demo server").
				WithWebsiteURL("https://demo.test").
				WithIcons(schema.NewIcon("/i.png").WithMimeType("image/png")),
			want: map[string]any{
				"name":        "demo",
				"version":     "1.0.0",
				"title":       "Demo",
				"description": "A demo server",
				"websiteUrl":  "https://demo.test",
				"icons": []map[string]any{
					{"src": "/i.png", "mimeType": "image/png"},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.impl.ToMap()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestImplementation_MarshalJSON(t *testing.T) {
	impl := schema.NewImplementation("demo", "1.0.0").WithTitle("Demo")
	b, err := json.Marshal(impl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{"name": "demo", "version": "1.0.0", "title": "Demo"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %#v, want %#v", m, want)
	}
}

func TestImplementation_EmptyIconsOmitted(t *testing.T) {
	impl := schema.NewImplementation("demo", "1.0.0").WithIcons()
	m := impl.ToMap()
	if _, ok := m["icons"]; ok {
		t.Fatalf("icons should be omitted when empty")
	}
}

func TestImplementation_WithHelpersDoNotMutateOriginal(t *testing.T) {
	base := schema.NewImplementation("demo", "1.0.0")
	_ = base.WithTitle("t").WithDescription("d").WithWebsiteURL("u").WithIcons(schema.NewIcon("i"))
	if base.Title != "" || base.Description != "" || base.WebsiteURL != "" || base.Icons != nil {
		t.Fatalf("With* mutated original: %#v", base)
	}
}
