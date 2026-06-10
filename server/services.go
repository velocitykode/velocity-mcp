package server

import "github.com/velocitykode/velocity/app"

// ExtensionKey is the key under which an MCP Server is registered in
// app.Services.Extensions, mirroring velocity-ai's "ai" extension convention.
const ExtensionKey = "mcp"

// RegisterServices stores srv in the application's service container under
// ExtensionKey so it can later be retrieved with FromServices and so velocity's
// bootstrap auto-wires its event dispatcher (Server implements
// contract.EventDispatcherAware). It returns an error when an MCP server is
// already registered, mirroring app.RegisterExtension semantics.
func RegisterServices(s *app.Services, srv *Server) error {
	return app.RegisterExtension(s, ExtensionKey, srv)
}

// FromServices returns the MCP Server registered in the application's service
// container, or nil when none is registered. It mirrors velocity-ai's
// FromServices accessor.
func FromServices(s *app.Services) *Server {
	if s == nil {
		return nil
	}
	srv, err := app.ExtensionAs[*Server](s, ExtensionKey)
	if err != nil {
		return nil
	}
	return srv
}
