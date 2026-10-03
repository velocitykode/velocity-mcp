package client

import (
	"sync"
	"time"

	"github.com/velocitykode/velocity/async"
)

// disconnectAllBound is how long DisconnectAll waits for its clients. It is the
// longest one stdio server may take to be stopped, stage by stage, and a
// second: the clients are disconnected side by side, so that is what all of
// them together may take.
const disconnectAllBound = 2*shutdownGrace + pipeGrace + time.Second

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
	existing, replaced := m.clients[name]
	delete(m.clients, name)
	m.factories[name] = factory
	m.mu.Unlock()
	// The client is disconnected once the registry is let go of: stopping a
	// stdio server takes as long as the server takes to exit, and no lookup of
	// another name has to wait for that.
	if replaced {
		existing.Disconnect()
	}
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
//
// The clients are disconnected side by side. Stopping a stdio server gives it
// time to exit by itself before it is asked and then made to, so one after
// another a registry of servers would take the sum of those waits; together
// they take the longest of them. The whole is bounded as well: a transport
// whose disconnect does not return is left behind after disconnectAllBound
// rather than holding the caller, which is usually a process shutting down.
func (m *Manager) DisconnectAll() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = map[string]*Client{}
	m.mu.Unlock()
	if len(clients) == 0 {
		return
	}

	_, _ = async.RunWithTimeout(disconnectAllBound, func() struct{} {
		async.ForEach(clients, len(clients), func(c *Client) { c.Disconnect() })
		return struct{}{}
	}).Get()
}
