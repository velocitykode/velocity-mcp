// Package velocitymcp is a first-party SDK for building MCP (Model Context
// Protocol) servers on the Velocity web framework.
//
// The root package contains no code; functionality lives in domain packages:
//
//   - jsonrpc:        JSON-RPC 2.0 message types (leaf)
//   - schema:         fluent JSON Schema builder for tool arguments (leaf)
//   - content:        MCP content types: text, image, audio, blob, resource link (leaf)
//   - server:         Server, primitives (Tool, Resource, Prompt), registrar
//   - server/methods: protocol method handlers (initialize, tools/call, ...)
//   - transport:      stdio and Velocity-router HTTP transports
//   - provider:       chain service provider (typed registration + /mcp route)
//   - event:          MCP events dispatched through Velocity's event system
//   - mcptest:        test helpers and fakes
package velocitymcp
