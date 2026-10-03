package server

import (
	"bytes"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// resultTypeComplete marks a result that carries the whole answer, as opposed
// to one that suspends the call awaiting further client input.
const resultTypeComplete = "complete"

// jsonResultTypeComplete is the pre-encoded JSON form of resultTypeComplete, so
// the envelope never has to marshal a constant on the request path.
var jsonResultTypeComplete = json.RawMessage(`"` + resultTypeComplete + `"`)

// applyResultEnvelope decorates a successful result with the members the
// protocol expects on a server result: a "resultType" describing whether the
// call finished, the server implementation metadata under
// _meta[MetaKeyServerInfo], and, on a complete result of a cacheable
// operation, the "ttlMs" and "cacheScope" caching hints (see caching.go).
//
// Scope: only a success result is decorated. An error response, a message that
// produces no reply, and a server-initiated notification are all left exactly
// as the handler built them. A handler that set its own resultType keeps it,
// and its own _meta keys survive alongside the server info, so a method can
// suspend a call or attach its own metadata without the envelope overwriting
// either. The same holds for the caching hints: a handler that priced its own
// result keeps the numbers it wrote. A result that is not a JSON object is
// passed through untouched rather than being reshaped.
//
// Only the two levels the envelope actually writes (the result object and its
// _meta object) are decoded, and only into raw JSON members. Every other byte
// the handler produced is carried over verbatim, so a payload is never
// renumbered: decoding a result into map[string]any would turn each JSON number
// into a float64 and silently round an id beyond 2^53 or a uint64 near its
// maximum on its way back out.
func applyResultEnvelope(c *Context, req *jsonrpc.Request, res HandleResult) HandleResult {
	if c == nil || res.Response == nil || res.Response.Error != nil || len(res.Response.Result) == 0 {
		return res
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(res.Response.Result, &result); err != nil || result == nil {
		return res
	}

	if _, ok := result["resultType"]; !ok {
		result["resultType"] = jsonResultTypeComplete
	}

	applyCacheHints(c, req, result)

	meta, err := envelopeMeta(c, result["_meta"])
	if err != nil {
		// The server implementation metadata is built from values this server
		// was constructed with, so a failure here is a server-side defect; keep
		// the original frame rather than dropping the reply.
		return res
	}
	result["_meta"] = meta

	encoded, err := json.Marshal(result)
	if err != nil {
		return res
	}
	res.Response.Result = encoded
	return res
}

// applyCacheHints writes the "ttlMs" and "cacheScope" members onto a complete
// result of a cacheable operation. A result the handler suspended awaiting
// client input is left alone: an interim result is not cacheable and carries no
// hints. A member the handler wrote itself is kept when it is a value the
// protocol defines, so a method can price its own result without the envelope
// overwriting it. One that is not (a negative, fractional or non-numeric ttlMs;
// a cacheScope outside the two the protocol names) is replaced by the zero
// hint's value for that member, because ttlMs is required to be an integer of
// zero or more and a scope a client cannot read must not be read as the wider
// one: the conservative value is the only one that cannot widen the audience or
// extend the lifetime of a result by a handler's slip.
func applyCacheHints(c *Context, req *jsonrpc.Request, result map[string]json.RawMessage) {
	if !resultTypeIsComplete(result["resultType"]) {
		return
	}
	hint, ok := cacheHintFor(c, req)
	if !ok {
		return
	}

	ttl, scope := hint.members()
	switch written, set := result["ttlMs"]; {
	case !set:
		result["ttlMs"] = ttl
	case !isNonNegativeInteger(written):
		result["ttlMs"], _ = CacheHint{}.members()
	}
	switch written, set := result["cacheScope"]; {
	case !set:
		result["cacheScope"] = scope
	case !isCacheScope(written):
		_, result["cacheScope"] = CacheHint{}.members()
	}
}

// isNonNegativeInteger reports whether a raw JSON member is the ttlMs the
// protocol defines: a number token made of digits alone, so no sign, fraction
// or exponent, and no other JSON type.
func isNonNegativeInteger(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return false
	}
	for _, b := range t {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// isCacheScope reports whether a raw JSON member is one of the two scope
// strings the protocol defines.
func isCacheScope(raw json.RawMessage) bool {
	var scope string
	if err := json.Unmarshal(raw, &scope); err != nil {
		return false
	}
	return CacheScope(scope) == CacheScopePublic || CacheScope(scope) == CacheScopePrivate
}

// resultTypeIsComplete reports whether a result's resultType member says the
// call finished. The member is decoded rather than compared as raw bytes, so a
// handler that wrote the same string differently is still read correctly.
func resultTypeIsComplete(raw json.RawMessage) bool {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value == resultTypeComplete
}

// envelopeMeta returns the result's _meta object with the server implementation
// metadata added to it. A handler's own keys are preserved as the raw JSON it
// produced; a _meta member that is absent, null, or not an object is replaced,
// because the envelope owns the key it writes and the protocol models _meta as
// an object.
func envelopeMeta(c *Context, existing json.RawMessage) (json.RawMessage, error) {
	meta := map[string]json.RawMessage{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &meta); err != nil || meta == nil {
			meta = map[string]json.RawMessage{}
		}
	}

	info, err := json.Marshal(c.Implementation().ToMap())
	if err != nil {
		return nil, err
	}
	meta[MetaKeyServerInfo] = info

	return json.Marshal(meta)
}
