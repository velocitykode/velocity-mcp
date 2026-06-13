package server

import "github.com/velocitykode/velocity/app"

// RegisterServices stores srv in the application's type-keyed component
// registry (key: *server.Server) so it can later be retrieved with
// FromServices and so velocity's bootstrap auto-wires its event dispatcher
// (Server implements contract.EventDispatcherAware). It returns an error when
// an MCP server is already registered, mirroring app.Register duplicate
// semantics. No string key: the type identity is collision-free by import
// path.
func RegisterServices(s *app.Services, srv *Server) error {
	return app.Register(s, srv)
}

// FromServices returns the MCP Server registered in the application's service
// container, or nil when none is registered.
func FromServices(s *app.Services) *Server {
	srv, err := app.Get[*Server](s)
	if err != nil {
		return nil
	}
	return srv
}
