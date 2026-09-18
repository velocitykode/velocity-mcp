package server

// WithExtensions advertises protocol extensions under the "extensions"
// capability. Each key is an extension identifier (such as ExtensionUI) and is
// advertised as an empty object, the form the specification uses for an
// extension that takes no configuration. Calling it more than once accumulates.
//
// The MCP Apps extension is advertised automatically when the server registers
// an app resource, so most servers never need this option.
func WithExtensions(extensions ...string) Option {
	return func(s *Server) {
		for _, name := range extensions {
			if name != "" {
				s.addExtension(name)
			}
		}
	}
}

// addExtension records an advertised extension under the "extensions"
// capability, creating the capability object on first use. Advertising the same
// extension twice is a no-op.
func (s *Server) addExtension(name string) {
	if s.capabilities == nil {
		s.capabilities = map[string]any{}
	}
	extensions, _ := s.capabilities[CapabilityExtensions].(map[string]any)
	if extensions == nil {
		extensions = map[string]any{}
		s.capabilities[CapabilityExtensions] = extensions
	}
	if _, ok := extensions[name]; !ok {
		extensions[name] = map[string]any{}
	}
}

// detectUICapability advertises the MCP Apps extension when any registered
// resource is an app resource. It runs once, after the options have been
// applied, so a server that registers an app resource never has to advertise
// the extension by hand.
func (s *Server) detectUICapability() {
	for _, r := range s.resources {
		if _, ok := r.(AppResource); ok {
			s.addExtension(ExtensionUI)
			return
		}
	}
	for _, t := range s.templates {
		if _, ok := t.(AppResource); ok {
			s.addExtension(ExtensionUI)
			return
		}
	}
}
