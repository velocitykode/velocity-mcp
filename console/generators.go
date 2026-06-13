package console

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"

	cli "github.com/velocitykode/velocity-cli"
	velapp "github.com/velocitykode/velocity/app"
	"github.com/velocitykode/velocity/chain"
	"github.com/velocitykode/velocity/console/scaffold"
)

//go:embed stubs/*.stub
var stubFS embed.FS

// generator is one make:mcp-* code generator. It is a chain.Command that
// scaffolds a primitive starter file into the user's project, delegating all
// path safety, directory resolution, and stub rendering to the framework's
// scaffold.Generator so behavior matches velocity's built-in make:* commands.
type generator struct {
	command     string // invoked as: vel run <command>
	description string
	kind        string // "tool" | "resource" | "prompt"
	defaultDir  string // default output directory, overridable with --dir
	stubPath    string // embedded stub file
	typeSuffix  string // appended to the derived Pascal name (e.g. "Tool")
}

// Generators returns the MCP code-generator commands. A service provider
// exposes them to an application by adding them to the command registry (see
// provider.Provider.Commands), after which they run as `vel run make:mcp-...`.
func Generators() []chain.Command {
	return []chain.Command{
		generator{
			command:     "make:mcp-tool",
			description: "Generate an MCP tool",
			kind:        "tool",
			defaultDir:  "internal/tools",
			stubPath:    "stubs/tool.go.stub",
			typeSuffix:  "Tool",
		},
		generator{
			command:     "make:mcp-resource",
			description: "Generate an MCP resource",
			kind:        "resource",
			defaultDir:  "internal/resources",
			stubPath:    "stubs/resource.go.stub",
			typeSuffix:  "Resource",
		},
		generator{
			command:     "make:mcp-prompt",
			description: "Generate an MCP prompt",
			kind:        "prompt",
			defaultDir:  "internal/prompts",
			stubPath:    "stubs/prompt.go.stub",
			typeSuffix:  "Prompt",
		},
	}
}

// Name implements chain.Command.
func (g generator) Name() string { return g.command }

// Description implements chain.Command.
func (g generator) Description() string { return g.description }

// Handle implements chain.Command: it parses the primitive name and optional
// --dir override, derives the type/identifier names, and writes the rendered
// stub. The scaffold.Generator enforces the traversal, symlink, and overwrite
// guards, so a malicious name or --dir is rejected exactly as for core make:*.
func (g generator) Handle(s *velapp.Services, args []string) error {
	name, dir, err := parseGenArgs(args, g.command)
	if err != nil {
		return err
	}

	stub, err := stubFS.ReadFile(g.stubPath)
	if err != nil {
		return fmt.Errorf("mcp: read %s stub: %w", g.kind, err)
	}

	base := scaffold.PascalCase(name)
	primitive := kebabCase(base)
	filename := scaffold.SnakeCase(base) + "_" + g.kind + ".go"

	// Resolve the output directory up front purely to derive the package name
	// for the rendered file. Generate re-resolves it internally to the same
	// value, so the file still lands through the audited write path.
	outDir, err := scaffold.ResolveDir(g.defaultDir, dir)
	if err != nil {
		return err
	}

	data := map[string]any{
		"Package":       filepath.Base(outDir),
		"Type":          base + g.typeSuffix,
		"PrimitiveName": primitive,
		"URI":           "velocity://" + primitive,
	}

	res, err := scaffold.Generator{
		DefaultDir: g.defaultDir,
		Kind:       g.kind,
		Stub:       string(stub),
		Filename:   filename,
	}.Generate(name, dir, data)
	if err != nil {
		return err
	}

	cli.Success(fmt.Sprintf("Created %s: %s", g.kind, res.Path))
	return nil
}

// parseGenArgs extracts the primitive name and optional --dir override from a
// command's arguments. Exactly one positional name is required; unknown flags
// are rejected so a typo does not silently become the name.
func parseGenArgs(args []string, command string) (name, dir string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dir":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--dir requires a directory value")
			}
			dir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--dir="):
			dir = strings.TrimPrefix(arg, "--dir=")
		case strings.HasPrefix(arg, "-"):
			return "", "", fmt.Errorf("unknown flag %q", arg)
		default:
			if name != "" {
				return "", "", fmt.Errorf("unexpected argument %q (only one name is allowed)", arg)
			}
			name = arg
		}
	}
	if name == "" {
		return "", "", fmt.Errorf("name is required (usage: vel run %s <Name> [--dir DIR])", command)
	}
	return name, dir, nil
}

// kebabCase converts a Pascal/snake identifier to the kebab-case form MCP uses
// for primitive names (e.g. "WeatherForecast" becomes "weather-forecast").
func kebabCase(s string) string {
	return strings.ReplaceAll(scaffold.SnakeCase(s), "_", "-")
}
