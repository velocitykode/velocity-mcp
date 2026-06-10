# Velocity MCP

Rapidly build MCP (Model Context Protocol) servers for your Velocity applications.

Velocity MCP is a first-party SDK that lets AI clients (Claude Code, Claude Desktop, Cursor, and others) interact with your Velocity application through the Model Context Protocol. Define tools, resources, and prompts with familiar Velocity conventions, then expose them over stdio or HTTP.

## Status

Early scaffold. API not yet stable (pre-1.0).

## Architecture

Native MCP protocol implementation built on Velocity framework components (router, validation, events). No third-party MCP SDK dependency.

| Package | Purpose |
|---------|---------|
| `jsonrpc` | JSON-RPC 2.0 message types |
| `schema` | Fluent JSON Schema builder for tool arguments |
| `content` | Content types: Text, Image, Audio, Blob, ResourceLink |
| `server` | Server core, primitives (Tool, Resource, Prompt), registrar |
| `server/methods` | Protocol method handlers |
| `transport` | Stdio and Velocity-router HTTP transports |
| `event` | MCP events via Velocity's event system |
| `mcptest` | Test helpers and fakes |

## Install

```bash
go get github.com/velocitykode/velocity-mcp
```

## License

MIT
