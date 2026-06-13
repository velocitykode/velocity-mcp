package client

import (
	"context"
	"time"

	"github.com/velocitykode/velocity-mcp/schema"
)

// Client is an MCP client bound to a single transport. It connects lazily: the
// first request that needs a connection performs the initialize handshake. A
// Client is safe for sequential use; the underlying protocol serializes the
// request/response exchange.
type Client struct {
	proto      *protocol
	transport  Transport
	clientInfo schema.Implementation
	name       string
}

// defaultClientInfo identifies this client to servers when no clientInfo is set.
func defaultClientInfo() schema.Implementation {
	return schema.NewImplementation("Velocity MCP Client", "0.1.0")
}

// New builds a Client over an arbitrary transport with the given client identity.
// A zero clientInfo (empty name) falls back to a default identity.
func New(transport Transport, clientInfo schema.Implementation) *Client {
	if clientInfo.Name == "" {
		clientInfo = defaultClientInfo()
	}
	return &Client{
		proto:      newProtocol(transport, clientInfo),
		transport:  transport,
		clientInfo: clientInfo,
	}
}

// Local builds a Client that talks to a server subprocess over stdio.
func Local(command string, args ...string) *Client {
	return New(NewStdioTransport(command, args...), schema.Implementation{})
}

// Web builds a WebClient that talks to a server over streamable HTTP, adding
// bearer-token and OAuth helpers.
func Web(url string) *WebClient {
	transport := NewHTTPTransport(url)
	return &WebClient{
		Client:    New(transport, schema.Implementation{}),
		transport: transport,
	}
}

// WithClientInfo overrides the client identity. It must be called before connect.
func (c *Client) WithClientInfo(info schema.Implementation) *Client {
	if info.Name == "" {
		info = defaultClientInfo()
	}
	c.clientInfo = info
	c.proto = newProtocol(c.transport, info)
	return c
}

// WithTimeout sets the per-operation timeout on the transport.
func (c *Client) WithTimeout(d time.Duration) *Client {
	c.transport.SetTimeout(d)
	return c
}

// ClientInfo returns the client identity sent on initialize.
func (c *Client) ClientInfo() schema.Implementation { return c.clientInfo }

// Connect performs the initialize handshake. It is optional: any request
// connects on demand.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.proto.connect(ctx); err != nil {
		return err
	}
	return nil
}

// Disconnect tears down the transport.
func (c *Client) Disconnect() { c.proto.disconnect() }

// Connected reports whether the initialize handshake has completed.
func (c *Client) Connected() bool { return c.proto.isConnected() }

// InitializeResult returns the server's initialize result, or nil before connect.
func (c *Client) InitializeResult() *InitializeResult { return c.proto.initializeResult() }

// Ping sends a ping request and waits for the empty acknowledgement.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.proto.dispatch(ctx, "ping", nil)
	return err
}

// Tools lists the server's tools, following pagination. An optional limit caps
// the number returned.
func (c *Client) Tools(ctx context.Context, limit ...int) ([]Tool, error) {
	entries, err := c.list(ctx, "tools", limit)
	if err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(entries))
	for _, e := range entries {
		t, err := parseTool(c, e)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// CallTool invokes a tool by name. A nil arguments map is sent as an empty object.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*ToolResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	raw, err := c.proto.dispatch(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	return parseToolResult(raw)
}

// Resources lists the server's resources, following pagination.
func (c *Client) Resources(ctx context.Context, limit ...int) ([]Resource, error) {
	entries, err := c.list(ctx, "resources", limit)
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(entries))
	for _, e := range entries {
		r, err := parseResource(e)
		if err != nil {
			return nil, err
		}
		resources = append(resources, r)
	}
	return resources, nil
}

// ReadResource reads a resource by URI.
func (c *Client) ReadResource(ctx context.Context, uri string) (*ResourceReadResult, error) {
	raw, err := c.proto.dispatch(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	return parseResourceReadResult(raw)
}

// Prompts lists the server's prompts, following pagination.
func (c *Client) Prompts(ctx context.Context, limit ...int) ([]Prompt, error) {
	entries, err := c.list(ctx, "prompts", limit)
	if err != nil {
		return nil, err
	}
	prompts := make([]Prompt, 0, len(entries))
	for _, e := range entries {
		p, err := parsePrompt(e)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, nil
}

// GetPrompt fetches a prompt by name. A nil arguments map is sent as an empty
// object.
func (c *Client) GetPrompt(ctx context.Context, name string, arguments map[string]any) (*PromptResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	raw, err := c.proto.dispatch(ctx, "prompts/get", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	return parsePromptResult(raw)
}
