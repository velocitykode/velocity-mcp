package server

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// CacheScope is the audience a cacheable result may be reused for. A result
// scoped public carries no caller-specific data, so any client, gateway, or
// caching proxy may serve it to any caller; a private one may only be reused
// inside the authorization context that fetched it.
type CacheScope string

const (
	// CacheScopePrivate keeps a cached result inside the authorization context
	// that fetched it. It is the default: a result is only ever shared across
	// callers when the server says so.
	CacheScopePrivate CacheScope = "private"
	// CacheScopePublic lets any client, gateway, or caching proxy reuse the
	// result for any caller. It belongs on a result that is identical for every
	// caller, never on one that depends on who asked.
	CacheScopePublic CacheScope = "public"
)

// CacheHint is the caching advice attached to the results of the operations the
// protocol defines as cacheable: "server/discover", "tools/list",
// "prompts/list", "resources/list", "resources/templates/list", and
// "resources/read". It travels as the "ttlMs" and "cacheScope" members of the
// result.
//
// The zero value is the conservative advice every server starts with: no
// lifetime, so the result is stale the moment it arrives, and private scope, so
// it is never shared across authorization contexts. Widen it with
// WithCacheHint for the whole server, WithMethodCacheHint for one operation, or
// by implementing Cacheable on a resource.
//
// A hint is a freshness hint and nothing more. It never stands in for access
// control: a primitive that must not be read by a caller is refused by the
// primitive, not by the scope of its cache entry.
type CacheHint struct {
	// TTL is how long a client may consider the result fresh. It is reported in
	// whole milliseconds, rounded down, so a lifetime shorter than a
	// millisecond reports 0; a negative lifetime also reports 0, the value the
	// protocol defines as immediately stale, because the field is required to
	// be zero or more.
	TTL time.Duration
	// Scope is the audience the result may be reused for. Any value other than
	// CacheScopePublic is reported as CacheScopePrivate, so neither the zero
	// value nor a misspelled one can widen the audience by accident.
	Scope CacheScope
}

// ttlMs renders the hint's lifetime in the unit the wire field is defined in.
func (h CacheHint) ttlMs() int64 {
	if h.TTL < time.Millisecond {
		return 0
	}
	return int64(h.TTL / time.Millisecond)
}

// scope returns the hint's audience, narrowing anything but an explicit public
// scope to private.
func (h CacheHint) scope() CacheScope {
	if h.Scope == CacheScopePublic {
		return CacheScopePublic
	}
	return CacheScopePrivate
}

// members renders the hint as the two result members it travels in. The scope
// comes from a closed set, so it is written as a JSON string directly.
func (h CacheHint) members() (ttl, scope json.RawMessage) {
	return json.RawMessage(strconv.FormatInt(h.ttlMs(), 10)),
		json.RawMessage(`"` + string(h.scope()) + `"`)
}

// Cacheable is implemented by a resource that carries its own caching advice.
// It overrides both the hint configured for "resources/read" and the
// server-wide one when that resource is the one being read: how long a
// resource stays fresh, and whether its contents depend on who asked for them,
// is a property of the resource rather than of the operation that reads it.
type Cacheable interface {
	CacheHint() CacheHint
}

// cacheableMethods are the operations whose complete results carry caching
// hints. Every other method answers without them.
var cacheableMethods = map[string]struct{}{
	"server/discover":          {},
	"tools/list":               {},
	"prompts/list":             {},
	"resources/list":           {},
	"resources/templates/list": {},
	"resources/read":           {},
}

// cacheHintFor resolves the caching advice for a request, reporting false when
// the request is not one whose results carry any.
//
// The most specific hint wins: the resource a read addresses when it declares
// one, then the hint configured for the operation, then the server-wide hint,
// then the zero hint.
//
// A request retried with client input is answered with the zero hint whatever
// is configured. Such a result depends on inputs that are not part of the cache
// key, so it must not be reused; a lifetime of 0 under private scope is how the
// protocol states that, and it keeps the members present, which results of
// these operations are required to be.
func cacheHintFor(c *Context, req *jsonrpc.Request) (CacheHint, bool) {
	if c == nil || req == nil {
		return CacheHint{}, false
	}
	if _, ok := cacheableMethods[req.Method]; !ok {
		return CacheHint{}, false
	}

	members := paramMembers(req.Params)
	if _, retried := members["inputResponses"]; retried {
		return CacheHint{}, true
	}
	if _, retried := members["requestState"]; retried {
		return CacheHint{}, true
	}

	if req.Method == "resources/read" {
		if resource, _ := c.ResolveResource(rawString(members["uri"])); resource != nil {
			if declared, ok := resource.(Cacheable); ok {
				return declared.CacheHint(), true
			}
		}
	}
	if hint, ok := c.methodCacheHints[req.Method]; ok {
		return hint, true
	}
	if c.cacheHint != nil {
		return *c.cacheHint, true
	}
	return CacheHint{}, true
}

// paramMembers decodes the top-level members of a request's params without
// decoding their values. The envelope reads three of them, and decoding the
// whole bag into map[string]any to reach them would turn every number the
// request carries into a float64 on the way.
func paramMembers(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil
	}
	return members
}

// rawString reads a raw JSON member as a string, returning "" for a member that
// is absent or is not one.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
