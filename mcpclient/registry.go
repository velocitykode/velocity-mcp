package mcpclient

import "sync"

// entry is a registered named MCP server: the URL of its streamable-HTTP
// endpoint.
type entry struct {
	resourceURL string
}

var registry = struct {
	mu sync.RWMutex
	m  map[string]entry
}{m: map[string]entry{}}

// RegisterClient records a named MCP server by its endpoint URL. Mirrors the
// "register a client by name" step: declare it once, then refer to it by name
// in OAuthRoutesFor and For.
func RegisterClient(name, resourceURL string) {
	registry.mu.Lock()
	registry.m[name] = entry{resourceURL: resourceURL}
	registry.mu.Unlock()
}

// lookup returns the registered entry for a name.
func lookup(name string) (entry, bool) {
	registry.mu.RLock()
	e, ok := registry.m[name]
	registry.mu.RUnlock()
	return e, ok
}
