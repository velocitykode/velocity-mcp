package server

// WithMethod registers a handler for a JSON-RPC method, either a method the
// protocol does not define or a replacement for one of the built-in handlers.
// A custom handler wins over the built-in of the same name, so a server can
// override, for instance, how it answers tools/list. An empty name or a nil
// handler is ignored.
//
// A custom handler is subject to the same rules as a built-in one: its result
// receives the server's result envelope, and it must report a protocol failure
// by returning a *jsonrpc.Error rather than panicking or leaking internal
// detail.
//
// Replacing "initialize" is allowed and keeps the surrounding handshake: a
// successful result still assigns a session id and dispatches
// event.SessionInitialized. The replacement then owns the version negotiation,
// so it records what it settled on with Context.SetNegotiatedVersion, which is
// what the event and the rest of the connection read.
func WithMethod(name string, m Method) Option {
	return func(s *Server) {
		if name == "" || m == nil {
			return
		}
		if s.customMethods == nil {
			s.customMethods = map[string]Method{}
		}
		s.customMethods[name] = m
	}
}
