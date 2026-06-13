package console

import (
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// genByKind returns the generator with the given kind for tests.
func genByKind(t *testing.T, kind string) generator {
	t.Helper()
	for _, c := range Generators() {
		g := c.(generator)
		if g.kind == kind {
			return g
		}
	}
	t.Fatalf("no generator for kind %q", kind)
	return generator{}
}

func TestGeneratorsScaffold(t *testing.T) {
	cases := []struct {
		kind     string
		wantPath string
		wantType string
		wantPkg  string
	}{
		{"tool", "internal/tools/weather_forecast_tool.go", "WeatherForecastTool", "tools"},
		{"resource", "internal/resources/weather_forecast_resource.go", "WeatherForecastResource", "resources"},
		{"prompt", "internal/prompts/weather_forecast_prompt.go", "WeatherForecastPrompt", "prompts"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := genByKind(t, tc.kind).Handle(nil, []string{"WeatherForecast"}); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			src, err := os.ReadFile(tc.wantPath)
			if err != nil {
				t.Fatalf("expected file %s: %v", tc.wantPath, err)
			}

			// Generated source must be syntactically valid Go and already
			// gofmt-clean, so the file a user gets compiles and passes CI.
			if _, err := parser.ParseFile(token.NewFileSet(), tc.wantPath, src, parser.AllErrors); err != nil {
				t.Fatalf("generated file does not parse: %v", err)
			}
			formatted, err := format.Source(src)
			if err != nil {
				t.Fatalf("format.Source: %v", err)
			}
			if string(formatted) != string(src) {
				t.Fatal("generated file is not gofmt-clean")
			}

			got := string(src)
			for _, want := range []string{
				"package " + tc.wantPkg,
				tc.wantType,
				`"weather-forecast"`, // kebab-case primitive name
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("generated file missing %q\n---\n%s", want, got)
				}
			}
		})
	}
}

func TestGeneratorDirOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := genByKind(t, "tool").Handle(nil, []string{"Weather", "--dir", "internal/mcp/tools"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	path := "internal/mcp/tools/weather_tool.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
	// Package name derives from the override directory's last segment.
	if !strings.Contains(string(src), "package tools") {
		t.Fatalf("package not derived from --dir: \n%s", src)
	}
}

func TestGeneratorRefusesOverwrite(t *testing.T) {
	t.Chdir(t.TempDir())
	g := genByKind(t, "tool")
	if err := g.Handle(nil, []string{"Weather"}); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if err := g.Handle(nil, []string{"Weather"}); err == nil {
		t.Fatal("second Handle overwrote an existing file, want error")
	}
}

func TestGeneratorRejectsUnsafeNames(t *testing.T) {
	t.Chdir(t.TempDir())
	g := genByKind(t, "tool")
	for _, name := range []string{"../escape", "..", "/abs", "with space", "semi;colon"} {
		if err := g.Handle(nil, []string{name}); err == nil {
			t.Fatalf("name %q accepted, want rejection", name)
		}
	}
	// A --dir traversal must also be refused.
	if err := g.Handle(nil, []string{"Weather", "--dir", "../../tmp"}); err == nil {
		t.Fatal("--dir traversal accepted, want rejection")
	}
}

func TestParseGenArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantName string
		wantDir  string
		wantErr  bool
	}{
		{"name only", []string{"Weather"}, "Weather", "", false},
		{"dir spaced", []string{"Weather", "--dir", "x/y"}, "Weather", "x/y", false},
		{"dir equals", []string{"Weather", "--dir=x/y"}, "Weather", "x/y", false},
		{"missing name", []string{"--dir", "x"}, "", "", true},
		{"dir no value", []string{"Weather", "--dir"}, "", "", true},
		{"unknown flag", []string{"Weather", "--force"}, "", "", true},
		{"two names", []string{"A", "B"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, dir, err := parseGenArgs(tc.args, "make:mcp-tool")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName || dir != tc.wantDir {
				t.Fatalf("got (%q,%q), want (%q,%q)", name, dir, tc.wantName, tc.wantDir)
			}
		})
	}
}

// ensure the embedded stubs match the kinds and stay non-empty.
func TestStubsEmbedded(t *testing.T) {
	for _, c := range Generators() {
		g := c.(generator)
		b, err := stubFS.ReadFile(g.stubPath)
		if err != nil {
			t.Fatalf("missing stub %s: %v", g.stubPath, err)
		}
		if !strings.Contains(string(b), "{{.PrimitiveName}}") {
			t.Fatalf("stub %s missing template fields", g.stubPath)
		}
		_ = filepath.Base(g.stubPath)
	}
}
