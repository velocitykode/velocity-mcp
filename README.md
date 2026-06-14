# Velocity MCP

**Build MCP servers and consume remote ones, natively in Go.** Velocity MCP is a first-party SDK for the [Model Context Protocol](https://modelcontextprotocol.io): expose your Velocity application's tools, resources, and prompts to AI clients like Claude Code, Claude Desktop, and Cursor, and call out to other MCP servers, OAuth and all, from your own handlers.

It is a complete, native protocol implementation built on Velocity's own router, validation, and events. No third-party MCP SDK, no Node sidecar.

```bash
go get github.com/velocitykode/velocity-mcp
```

> **Status:** pre-1.0. The API is still settling and may change between minor versions.

## Why Velocity MCP

- **Server and client in one SDK.** Most MCP libraries do one side. This does both: serve your app to agents, and turn your app into an agent that consumes other MCP servers.
- **Fluent, type-safe primitives.** Define tools with a schema builder and a typed request, not hand-rolled JSON Schema.
- **OAuth that just works.** The client speaks RFC 9728 discovery, PKCE, and dynamic client registration. Mount the authorization-code routes with one provider; tokens persist in the session automatically.
- **Two transports.** Serve over stdio for desktop clients, or over HTTP on your existing Velocity router.
- **Built on Velocity.** Validation, events, and routing come from the framework you already use, so it stays light and consistent.

## Build a Server

Define a tool, register it, and serve over stdio:

```go
package main

import (
    "context"

    "github.com/velocitykode/velocity-mcp/schema"
    "github.com/velocitykode/velocity-mcp/server"
    "github.com/velocitykode/velocity-mcp/transport"
    _ "github.com/velocitykode/velocity-mcp/server/methods" // installs the protocol method set
)

func addTool() server.Tool {
    return server.NewTool("add", "Add two numbers").
        WithSchema(func(s *schema.Object) {
            s.Number("a").Required()
            s.Number("b").Required()
        }).
        HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
            return server.Text(formatFloat(req.Float("a") + req.Float("b"))), nil
        })
}

func main() {
    s := server.New("calc", "1.0.0", server.WithTools(addTool()))
    transport.ServeStdio(context.Background(), s)
}
```

Want HTTP instead? Mount the same server on your Velocity router with the Web transport, no rewrite required.

## Consume a Server

The `mcpclient` package is the ergonomic front door for calling MCP servers from a Velocity web app. Register a server once, mount its OAuth routes, and any handler can get an authorized client:

```go
mcpclient.RegisterClient("example", "https://mcp.example.com/mcp")

func Configure(reg *velocity.ProviderRegistry) {
    reg.Add(mcpclient.OAuthRoutesFor("example", oauth.Config{
        ClientID: "veladmin",
        Scope:    "mcp:use",
    }))
}

// later, inside a handler:
w, _ := mcpclient.For(c, "example") // *client.WebClient, bearer token attached
tools, _ := w.Tools(c.Request.Context())
```

`OAuthRoutesFor` mounts a redirect and a callback route on the session-backed web stack; after the user authorizes, the token is stored (velocity session by default, pluggable via `WithStore`) and reused on every call.

For non-web use, the lower-level `client` package gives you transports, the protocol engine, and OAuth helpers directly.

## Packages

| Package | Purpose |
|---------|---------|
| `jsonrpc` | JSON-RPC 2.0 message types |
| `schema` | Fluent JSON Schema builder for tool arguments |
| `content` | Content types: Text, Image, Audio, Blob, ResourceLink |
| `server` | Server core, primitives (Tool, Resource, Prompt), registrar |
| `server/methods` | Protocol method handlers |
| `transport` | Stdio and Velocity-router HTTP transports |
| `client` | Low-level client: transports, protocol, OAuth (discovery, PKCE, registration) |
| `mcpclient` | App-facing client integration: named servers, OAuth routes, token stores |
| `event` | MCP events via Velocity's event system |
| `mcptest` | Test helpers and fakes |

## Documentation

[vel.build/docs/ecosystem/velocity-mcp](https://vel.build/docs/ecosystem/velocity-mcp)

## License

MIT
