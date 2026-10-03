package server

import (
	"encoding/hex"
	"strings"

	"github.com/velocitykode/velocity/crypto"
	"github.com/velocitykode/velocity/str"
	"github.com/velocitykode/velocity/webhook"
)

// Session ids are self-authenticating. The server keeps no record of the
// sessions it opened (the HTTP transport is stateless and a record would be a
// map a client could fill), so an id it issued carries a tag only this server
// can produce: an HMAC-SHA256 over the id under the server's session key. That
// key is drawn at random at construction unless the application supplies one
// with WithSessionKey, which is what lets several instances of one deployment
// (replicas behind a load balancer, or one instance across a restart) vouch for
// each other's ids. A transport that cannot vouch for an inbound
// Mcp-Session-Id by itself asks IssuedSessionID, and hands application code only an id this server opened.
// Session ids belong to the initialize handshake (2025-11-25, session
// management), whose security guidance is that a server verify the ids it
// accepts rather than trust the header.
const (
	// sessionKeySize is the length of a drawn HMAC key. The key is drawn from
	// a 62 character alphabet, so 43 characters carry just over 256 bits.
	sessionKeySize = 43
	// sessionTagSize is the number of HMAC bytes kept as the tag: 128 bits,
	// which is as unforgeable as the id is unguessable.
	sessionTagSize = 16
	// sessionTagSeparator joins the generated id and its tag. The id part may
	// itself contain it; the tag is read from the last occurrence.
	sessionTagSeparator = "."
	// sessionKeyLabel is what a supplied key is keyed over to produce the key
	// session ids are tagged under, so a secret the application also uses
	// elsewhere never signs a session id directly.
	sessionKeyLabel = "velocity-mcp session id key v1"
	// MinSessionKeySize is the shortest key WithSessionKey accepts, in bytes.
	// A session id shows a tag made under the key, so a key short enough to
	// search can be recovered from one issued id and used to forge others;
	// 32 bytes is the output size of the HMAC and puts that out of reach.
	MinSessionKeySize = 32
	// maxSessionIDLength bounds the id a verifier is willing to hash. An issued
	// id is far shorter, and a session id is required to be visible ASCII, so
	// nothing longer can have come from this server.
	maxSessionIDLength = 512
)

// newSessionKey draws the per-server HMAC key from the framework's
// cryptographic random source. Like randomSessionID it returns nil rather than
// panicking should that source fail; a server without a key issues untagged
// ids and vouches for none of them, which is the safe side.
func newSessionKey() []byte {
	key, err := str.Random(sessionKeySize)
	if err != nil {
		return nil
	}
	return []byte(key)
}

// WithSessionKey supplies the secret the server tags and verifies session ids
// under, in place of the key each server otherwise draws at random for itself.
//
// Supply the same key to every instance that serves one deployment. A session
// id is verified by the instance that receives it, not the one that issued it,
// so without a shared key an id issued by one replica, or by this instance
// before a restart, is not recognised by another, and the request is handled as
// if it carried no session (Request.SessionID is empty). Instances given the
// same key vouch for each other's ids; instances given different keys, or none,
// do not.
//
// The key must be a secret of at least MinSessionKeySize (32) random bytes that
// is kept out of source control. The server does not sign with it directly: it
// derives the signing key from it, so the application's existing secret can be
// supplied without the two uses interfering. The slice is not retained.
//
// A key shorter than MinSessionKeySize never signs an id: one issued id would
// be enough to search for it and forge others. Such a key is refused, the
// server keeps the random key it drew for itself (so it still vouches only for
// its own ids, and no other instance vouches for them), and New reports the
// refusal once as an error through the logger given with WithLogger, naming
// the length supplied and the length required but never the key. New returns
// no error and does not panic, so a server built without a logger refuses the
// key just the same but has nowhere to say so.
//
// An empty or nil key is refused and reported in the same way, as a key of 0
// bytes: an application that uses this option means to supply a key, and a
// secret that arrived empty (an unset environment variable, say) would
// otherwise leave every instance on its own random key with nothing said.
func WithSessionKey(key []byte) Option {
	return func(s *Server) {
		if len(key) < MinSessionKeySize {
			s.sessionKeyRefused = true
			s.refusedSessionKeySize = len(key)
			return
		}
		s.sessionKey = webhook.HMACSHA256.Sign(key, []byte(sessionKeyLabel))
	}
}

// reportRefusedSessionKey logs, once and after every option has been applied
// (so the logger is the one the server ends up with whatever the option order),
// that a session key was refused for being too short.
func (s *Server) reportRefusedSessionKey() {
	if !s.sessionKeyRefused || s.logger == nil {
		return
	}
	s.logger.Error("mcp: session key refused as too short; session ids are signed under a per-process random key instead",
		"bytes", s.refusedSessionKeySize, "minimum", MinSessionKeySize)
}

// tagSessionID appends the tag that makes an id verifiable. An empty id (a
// generator or random source that produced nothing) stays empty, so the
// transport treats it as no session rather than as an id consisting of a tag.
func (s *Server) tagSessionID(id string) string {
	if id == "" || len(s.sessionKey) == 0 {
		return id
	}
	return id + sessionTagSeparator + s.sessionTag(id)
}

// sessionTag computes the hex tag for an id under the server's key.
func (s *Server) sessionTag(id string) string {
	mac := webhook.HMACSHA256.Sign(s.sessionKey, []byte(id))
	return hex.EncodeToString(mac[:sessionTagSize])
}

// IssuedSessionID reports whether id is a session id this server issued: one
// returned as HandleResult.SessionID by an initialize it answered, carrying the
// tag the server attached and no other change. The comparison runs in constant
// time, so a verifier cannot be used to learn a tag byte by byte.
//
// A transport that reads session ids off the wire calls this before passing one
// on, so that Request.SessionID and the tool events name a session the server
// actually opened rather than whatever a client wrote in a header. An id this
// server did not issue, an id issued by an instance that does not share this
// server's key (see WithSessionKey), an empty id, and anything that is not visible ASCII (the only
// characters a session id may contain) all report false.
func (s *Server) IssuedSessionID(id string) bool {
	if id == "" || len(id) > maxSessionIDLength || len(s.sessionKey) == 0 || !isVisibleASCII(id) {
		return false
	}
	at := strings.LastIndex(id, sessionTagSeparator)
	if at <= 0 {
		return false
	}
	raw, tag := id[:at], id[at+len(sessionTagSeparator):]
	return crypto.EqualString(tag, s.sessionTag(raw))
}

// isVisibleASCII reports whether every byte of s is in the range a session id
// may use, 0x21 to 0x7E.
func isVisibleASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7E {
			return false
		}
	}
	return true
}
