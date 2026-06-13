// Package console provides the MCP code-generator commands (make:mcp-tool,
// make:mcp-resource, make:mcp-prompt) that scaffold primitive starter files
// into a user's project.
//
// The commands are chain.Command values exposed via Generators. A service
// provider adds them to the application's command registry, after which they
// run as `vel run make:mcp-...`. All file writing delegates to velocity's
// console/scaffold.Generator, so directory resolution, the --dir override,
// path-traversal and symlink guards, and overwrite protection match the
// framework's built-in make:* commands exactly. The package owns only what is
// domain-specific: the command names, the embedded stub templates, and the
// template data derived from the primitive name.
package console
