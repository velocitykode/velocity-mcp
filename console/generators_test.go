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
		{"server", "internal/servers/weather_forecast_server.go", "WeatherForecastServer", "servers"},
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

// The server generator scaffolds the container the other generators' output is
// registered on, so its file must name the constructor, the server identity,
// and the registration entry points a user then fills in.
func TestServerGeneratorScaffoldsRegistrationEntryPoints(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := genByKind(t, "server").Handle(nil, []string{"WeatherForecast"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	src, err := os.ReadFile("internal/servers/weather_forecast_server.go")
	if err != nil {
		t.Fatalf("expected generated server: %v", err)
	}
	got := string(src)
	for _, want := range []string{
		"func NewWeatherForecastServer() *server.Server",
		`server.New("weather-forecast", "0.0.1",`,
		"server.WithInstructions(",
		"server.WithTools(",
		"server.WithResources(",
		"server.WithPrompts(",
		`"github.com/velocitykode/velocity-mcp/server"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("generated server missing %q\n---\n%s", want, got)
		}
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

// A symlink at the target, dangling or not, must never be written through:
// the generated source would land wherever it points.
func TestGeneratorRefusesSymlinkAtTarget(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		t.Chdir(t.TempDir())
		outside := filepath.Join(t.TempDir(), "outside.go")
		if !dangling {
			if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll("internal/tools", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, "internal/tools/weather_tool.go"); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if err := genByKind(t, "tool").Handle(nil, []string{"Weather"}); err == nil {
			t.Fatalf("dangling=%v: generator wrote through a symlink, want error", dangling)
		}
		got, err := os.ReadFile(outside)
		if dangling && !os.IsNotExist(err) {
			t.Fatalf("dangling=%v: the link's destination was created: %q", dangling, got)
		}
		if !dangling && string(got) != "keep" {
			t.Fatalf("dangling=%v: the link's destination was changed to %q", dangling, got)
		}
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

// Every generator must be invocable: a unique name and a description for the
// help output.
func TestGeneratorsExposeUniqueNamesAndDescriptions(t *testing.T) {
	want := map[string]bool{
		"make:mcp-server":   false,
		"make:mcp-tool":     false,
		"make:mcp-resource": false,
		"make:mcp-prompt":   false,
	}
	for _, c := range Generators() {
		name := c.Name()
		seen, known := want[name]
		if !known {
			t.Fatalf("unexpected generator %q", name)
		}
		if seen {
			t.Fatalf("generator %q registered twice", name)
		}
		want[name] = true
		if c.Description() == "" {
			t.Fatalf("generator %q has no description", name)
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("generator %q not registered", name)
		}
	}
}

// The output directory's last segment names the package, and velocity's
// scaffold accepts segments that are not Go identifiers: a hyphen, an upper
// case letter or a keyword all pass its directory rule. The generated file must compile for
// every directory the generator accepts, so the package name is derived as an
// identifier where one can be, and a segment that cannot name a package is
// refused before anything is written.
func TestGeneratorPackageNameIsAGoIdentifier(t *testing.T) {
	t.Run("derived from the directory", func(t *testing.T) {
		cases := []struct {
			dir     string
			wantPkg string
		}{
			{"internal/my-tools", "package mytools"},
			{"internal/Mixed-Case_Dir", "package mixedcase_dir"},
			{"internal/My_Tools", "package my_tools"},
			{"internal/tools-2", "package tools2"},
		}
		for _, tc := range cases {
			t.Run(tc.dir, func(t *testing.T) {
				t.Chdir(t.TempDir())
				if err := genByKind(t, "tool").Handle(nil, []string{"Weather", "--dir", tc.dir}); err != nil {
					t.Fatalf("Handle: %v", err)
				}
				path := filepath.Join(tc.dir, "weather_tool.go")
				src, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("expected file %s: %v", path, err)
				}
				if _, err := parser.ParseFile(token.NewFileSet(), path, src, parser.AllErrors); err != nil {
					t.Fatalf("generated file does not parse: %v\n---\n%s", err, src)
				}
				if !strings.Contains(string(src), tc.wantPkg+"\n") {
					t.Fatalf("package clause: want %q in\n%s", tc.wantPkg, src)
				}
			})
		}
	})

	t.Run("a keyword segment is refused before writing", func(t *testing.T) {
		for _, dir := range []string{"internal/go", "internal/func", "internal/type", "internal/Go"} {
			t.Run(dir, func(t *testing.T) {
				t.Chdir(t.TempDir())
				err := genByKind(t, "tool").Handle(nil, []string{"Weather", "--dir", dir})
				if err == nil {
					t.Fatal("a keyword package name was accepted, want an error")
				}
				if !strings.Contains(err.Error(), "keyword") {
					t.Fatalf("error = %v, want it to name the keyword", err)
				}
				var written []string
				_ = filepath.WalkDir(".", func(path string, d os.DirEntry, _ error) error {
					if d != nil && !d.IsDir() {
						written = append(written, path)
					}
					return nil
				})
				if len(written) != 0 {
					t.Fatalf("files written despite the refusal: %v", written)
				}
			})
		}
	})
}

// A directory whose last segment gives the package name "main" is refused by
// every generator before anything is written: the file would declare a type in
// a program package, which nothing can import and which does not build as a
// library. The name is derived, so the refusal holds for every spelling that
// derives to it.
func TestGeneratorRefusesAProgramPackage(t *testing.T) {
	dirs := []string{"internal/main", "internal/Main", "internal/ma-in", "main", "cmd/server/main"}
	for _, kind := range []string{"tool", "resource", "prompt", "server"} {
		for _, dir := range dirs {
			t.Run(kind+"/"+dir, func(t *testing.T) {
				t.Chdir(t.TempDir())
				err := genByKind(t, kind).Handle(nil, []string{"Weather", "--dir", dir})
				if err == nil {
					t.Fatal("a directory naming the main package was accepted, want an error")
				}
				if !strings.Contains(err.Error(), `names the package "main"`) || !strings.Contains(err.Error(), "cannot be imported as a library") {
					t.Fatalf("error = %v, want it to say the directory names the main package, which is not a library", err)
				}
				entries, readErr := os.ReadDir(".")
				if readErr != nil {
					t.Fatalf("read the working directory: %v", readErr)
				}
				if len(entries) != 0 {
					t.Fatalf("the refusal left %d entries behind, the first %q", len(entries), entries[0].Name())
				}
			})
		}
	}
}

// The names packageNameFor refuses, and the ones next to them it must keep.
func TestPackageNameFor(t *testing.T) {
	cases := []struct {
		dir     string
		want    string
		wantErr string
	}{
		{dir: "internal/tools", want: "tools"},
		{dir: "internal/maintenance", want: "maintenance"},
		{dir: "internal/main2", want: "main2"},
		{dir: "internal/main_tools", want: "main_tools"},
		{dir: "internal/main", wantErr: "is a program"},
		{dir: "internal/MAIN", wantErr: "is a program"},
		{dir: "internal/m.a.i.n", wantErr: "is a program"},
		{dir: "internal/_", wantErr: "no Go package name"},
		{dir: "internal/-_-", wantErr: "no Go package name"},
		{dir: "internal/---", wantErr: "no Go package name"},
		{dir: "internal/2fa", wantErr: "no Go package name"},
		{dir: "internal/range", wantErr: "keyword"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			got, err := packageNameFor(tc.dir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("packageNameFor(%q) = %q, %v; want an error saying %q", tc.dir, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("packageNameFor(%q) = %q, %v; want %q", tc.dir, got, err, tc.want)
			}
		})
	}
}
