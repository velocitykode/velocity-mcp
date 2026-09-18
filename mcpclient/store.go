package mcpclient

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

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

// pendingLifetime is how long MemoryStore keeps a pending authorization. It
// covers the time a user spends at the authorization server plus the life of
// the code that comes back, which RFC 6749 4.1.2 recommends capping at ten
// minutes. A callback arriving later than this belongs to a flow that was
// abandoned, and the PKCE verifier and client secret kept for it have no
// business outliving it.
const pendingLifetime = 15 * time.Minute

// maxPendingAuthorizations bounds how many pending authorizations MemoryStore
// holds at once. Starting a flow costs a visitor one request and nothing else,
// so without a bound the redirect route is a way to grow this process's memory
// for as long as pendingLifetime allows. The oldest record makes room.
const maxPendingAuthorizations = 1024

// MemoryStore is a self-contained, process-local Store for apps without
// velocity sessions. It keys tokens to a browser via its own cookie. Suitable
// for single-process development; use a session- or cache-backed Store in
// production (tokens are lost on restart and not shared across instances).
//
// A pending authorization is dropped once it is taken, once pendingLifetime has
// passed, or when maxPendingAuthorizations newer ones have arrived.
type MemoryStore struct {
	cookieName string
	now        func() time.Time
	mu         sync.Mutex
	pending    map[string]pendingEntry // sid/state -> pending authorization
	tokens     map[string]string       // sid/name -> token
}

// pendingEntry is one stored pending authorization and the moment it lapses.
type pendingEntry struct {
	encoded   string
	expiresAt time.Time
}

// NewMemoryStore builds an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		cookieName: "mcp_oauth_sid",
		now:        time.Now,
		pending:    map[string]pendingEntry{},
		tokens:     map[string]string{},
	}
}

// sid returns the browser session id, minting and setting the cookie if absent.
// The cookie is velocity's canonical one (Path=/, HttpOnly, SameSite=Lax) and
// carries the Secure attribute on the framework's own terms: always, unless the
// application's validated session-cookie configuration opted out, which
// velocity permits in development and test profiles only. The id is all that
// stands between another party and this browser's token, so it is not sent over
// a connection anyone on the path can read.
func (m *MemoryStore) sid(c *router.Context) string {
	if ck, err := c.Cookie(m.cookieName); err == nil && ck.Value != "" {
		return ck.Value
	}
	id := newID()
	s := c.ServicesIfSet()
	c.SetCookie(router.FlashCookie(m.cookieName, id, 0, s == nil || !s.InsecureFlashCookies))
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
	key := m.sid(c) + "/" + p.State
	now := m.now()
	m.mu.Lock()
	m.dropLapsed(now)
	m.pending[key] = pendingEntry{encoded: string(b), expiresAt: now.Add(pendingLifetime)}
	m.mu.Unlock()
	return nil
}

// dropLapsed removes the pending authorizations whose lifetime has passed and,
// when the store is still full, the oldest of the rest, so that adding one more
// never takes it past maxPendingAuthorizations. The caller holds mu.
func (m *MemoryStore) dropLapsed(now time.Time) {
	for key, entry := range m.pending {
		if !now.Before(entry.expiresAt) {
			delete(m.pending, key)
		}
	}
	for len(m.pending) >= maxPendingAuthorizations {
		oldest, found := "", false
		for key, entry := range m.pending {
			if !found || entry.expiresAt.Before(m.pending[oldest].expiresAt) {
				oldest, found = key, true
			}
		}
		delete(m.pending, oldest)
	}
}

func (m *MemoryStore) TakePending(c *router.Context, state string) (*oauth.PendingAuthorization, error) {
	ck, err := c.Cookie(m.cookieName)
	if err != nil || ck.Value == "" {
		return nil, nil
	}
	key := ck.Value + "/" + state
	now := m.now()
	m.mu.Lock()
	entry, ok := m.pending[key]
	delete(m.pending, key)
	m.mu.Unlock()
	if !ok || !now.Before(entry.expiresAt) {
		return nil, nil
	}
	var p oauth.PendingAuthorization
	if err := json.Unmarshal([]byte(entry.encoded), &p); err != nil {
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
