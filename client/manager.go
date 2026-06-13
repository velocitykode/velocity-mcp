package client

import "sync"

// Manager is a registry of named MCP clients built lazily from factories. It
// memoizes each client so repeated lookups by name share one connection, and
// disconnects a prior instance when a name is re-registered.
type Manager struct {
	mu        sync.Mutex
	factories map[string]func() *Client
	clients   map[string]*Client
}

// NewManager builds an empty Manager.
func NewManager() *Manager {
	return &Manager{
		factories: map[string]func() *Client{},
		clients:   map[string]*Client{},
	}
}

// Register associates a name with a factory. Re-registering a name disconnects
// and discards any client previously built for it.
func (m *Manager) Register(name string, factory func() *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.clients[name]; ok {
		existing.Disconnect()
		delete(m.clients, name)
	}
	m.factories[name] = factory
}

// Client returns the memoized client for a name, building it on first access.
func (m *Manager) Client(name string) (*Client, error) {
	m.mu.Lock()
	if c, ok := m.clients[name]; ok {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	c, err := m.Build(name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.clients[name] = c
	m.mu.Unlock()
	return c, nil
}

// Build constructs a fresh (non-memoized) client for a name.
func (m *Manager) Build(name string) (*Client, error) {
	m.mu.Lock()
	factory, ok := m.factories[name]
	m.mu.Unlock()
	if !ok {
		return nil, newError("MCP client [" + name + "] has not been registered")
	}
	c := factory()
	c.name = name
	return c, nil
}

// DisconnectAll disconnects every memoized client and clears the cache.
func (m *Manager) DisconnectAll() {
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.mu.Unlock()
	for _, c := range clients {
		c.Disconnect()
	}
}
