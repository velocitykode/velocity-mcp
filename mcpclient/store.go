package mcpclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/velocitykode/velocity/auth"
	"github.com/velocitykode/velocity/router"
	"github.com/velocitykode/velocity/str"

	"github.com/velocitykode/velocity-mcp/client/oauth"
)

// Store persists the per-browser OAuth state across the authorization-code
// round trip: the short-lived PendingAuthorization (keyed by its state) and the
// resulting access token (keyed by client name). Implementations key everything
// to the current browser via the router.Context.
type Store interface {
	SavePending(c *router.Context, p *oauth.PendingAuthorization) error
	// TakePending returns and removes the pending authorization for a state,
	// or (nil, nil) when none is found.
	TakePending(c *router.Context, state string) (*oauth.PendingAuthorization, error)
	SaveToken(c *router.Context, name, token string) error
	// Token returns the stored access token for a name, or "" when absent.
	Token(c *router.Context, name string) (string, error)
}

// errNoSession is returned by SessionStore when the request has no velocity
// session (no auth/session middleware configured).
var errNoSession = errors.New("mcpclient: no velocity session available; configure sessions or use WithStore(NewMemoryStore())")

const (
	pendingKeyPrefix = "mcp.oauth.pending."
	tokenKeyPrefix   = "mcp.oauth.token."
)

// SessionStore persists OAuth state in the velocity session. It is the default
// store and requires the routes to run on the session-backed web stack (which
// OAuthRoutesFor uses).
type SessionStore struct{}

// session returns the active velocity session for the request, or nil.
func (SessionStore) session(c *router.Context) auth.Session {
	m := auth.FromContext(c)
	if m == nil {
		return nil
	}
	return m.Session(c.Request)
}

// SavePending stores the pending authorization as JSON under its state.
func (s SessionStore) SavePending(c *router.Context, p *oauth.PendingAuthorization) error {
	sess := s.session(c)
	if sess == nil {
		return errNoSession
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	sess.Put(pendingKeyPrefix+p.State, string(b))
	return nil
}

// TakePending decodes and removes the pending authorization for a state.
func (s SessionStore) TakePending(c *router.Context, state string) (*oauth.PendingAuthorization, error) {
	sess := s.session(c)
	if sess == nil {
		return nil, errNoSession
	}
	key := pendingKeyPrefix + state
	v := sess.Get(key)
	if v == nil {
		return nil, nil
	}
	sess.Remove(key)
	str, _ := v.(string)
	var p oauth.PendingAuthorization
	if err := json.Unmarshal([]byte(str), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveToken stores an access token under the client name.
func (s SessionStore) SaveToken(c *router.Context, name, token string) error {
	sess := s.session(c)
	if sess == nil {
		return errNoSession
	}
	sess.Put(tokenKeyPrefix+name, token)
	return nil
}

// Token returns the stored access token for a client name.
func (s SessionStore) Token(c *router.Context, name string) (string, error) {
	sess := s.session(c)
	if sess == nil {
		return "", errNoSession
	}
	str, _ := sess.Get(tokenKeyPrefix + name).(string)
	return str, nil
}

// MemoryStore is a self-contained, process-local Store for apps without
// velocity sessions. It keys tokens to a browser via its own cookie. Suitable
// for single-process development; use a session- or cache-backed Store in
// production (tokens are lost on restart and not shared across instances).
type MemoryStore struct {
	cookieName string
	mu         sync.Mutex
	pending    map[string]string // state -> pending JSON
	tokens     map[string]string // sid/name -> token
}

// NewMemoryStore builds an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		cookieName: "mcp_oauth_sid",
		pending:    map[string]string{},
		tokens:     map[string]string{},
	}
}

// sid returns the browser session id, minting and setting the cookie if absent.
func (m *MemoryStore) sid(c *router.Context) string {
	if ck, err := c.Cookie(m.cookieName); err == nil && ck.Value != "" {
		return ck.Value
	}
	id := newID()
	c.SetCookie(&http.Cookie{Name: m.cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return id
}

// newID returns a random browser-session identifier via velocity's str.Random.
func newID() string {
	id, _ := str.Random(32)
	return id
}

func (m *MemoryStore) SavePending(c *router.Context, p *oauth.PendingAuthorization) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// Bind the pending entry to this browser (sid) so only the browser that
	// began the flow can complete it, not just anyone presenting the state.
	m.mu.Lock()
	m.pending[m.sid(c)+"/"+p.State] = string(b)
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) TakePending(c *router.Context, state string) (*oauth.PendingAuthorization, error) {
	ck, err := c.Cookie(m.cookieName)
	if err != nil || ck.Value == "" {
		return nil, nil
	}
	key := ck.Value + "/" + state
	m.mu.Lock()
	str, ok := m.pending[key]
	delete(m.pending, key)
	m.mu.Unlock()
	if !ok {
		return nil, nil
	}
	var p oauth.PendingAuthorization
	if err := json.Unmarshal([]byte(str), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *MemoryStore) SaveToken(c *router.Context, name, token string) error {
	sid := m.sid(c)
	m.mu.Lock()
	m.tokens[sid+"/"+name] = token
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Token(c *router.Context, name string) (string, error) {
	ck, err := c.Cookie(m.cookieName)
	if err != nil || ck.Value == "" {
		return "", nil
	}
	m.mu.Lock()
	t := m.tokens[ck.Value+"/"+name]
	m.mu.Unlock()
	return t, nil
}

// defaultStore is used by OAuthRoutesFor (unless overridden with WithStore) and
// by the package-level For/Token helpers.
var defaultStore Store = SessionStore{}

// SetDefaultStore overrides the process-wide default Store. Call it before
// registering routes (e.g. SetDefaultStore(NewMemoryStore()) for an app without
// velocity sessions).
func SetDefaultStore(s Store) { defaultStore = s }
