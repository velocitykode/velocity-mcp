package oauth

import "strings"

// Challenge is the parsed content of a server's WWW-Authenticate response
// header (RFC 9728 §5.1). Every field is optional.
type Challenge struct {
	// ResourceMetadataURL is the advertised protected-resource metadata URL.
	ResourceMetadataURL string
	// Error is the OAuth error code, if any (e.g. "invalid_token").
	Error string
	// ErrorDescription is the human-readable error description, if any.
	ErrorDescription string
	// Scope is the space-delimited scope the server requires, if advertised.
	Scope string
}

// bearerScheme is the authentication scheme an MCP server challenges with.
const bearerScheme = "Bearer"

// ParseChallenge parses a WWW-Authenticate header value into a Challenge.
// A missing or empty header yields a zero-value Challenge (no error), matching
// servers that decline to advertise discovery metadata.
//
// The header is read as RFC 9110 11.6.1 defines it: one or more challenges,
// each a scheme followed by auth-params whose values are tokens or
// quoted-strings, with a backslash inside a quoted-string escaping the character
// after it. That matters beyond tidiness. A server that carries text it does not
// control in a realm or an error description escapes it that way, and a reader
// that ended the value at the first quote would go on to read the rest of that
// text as parameters, a metadata URL among them.
//
// The parameters of the Bearer challenge are the ones reported when the header
// holds several challenges, and those of every challenge when none is a Bearer
// one. A parameter stated more than once keeps its first value, and a value
// whose quoted-string never ends is dropped along with whatever follows it.
func ParseChallenge(header string) *Challenge {
	params := challengeParams(header)
	return &Challenge{
		ResourceMetadataURL: params["resource_metadata"],
		Error:               params["error"],
		ErrorDescription:    params["error_description"],
		Scope:               params["scope"],
	}
}

// challengeParams returns the auth-params a Challenge is built from, keyed by
// lower-cased name: those of the first Bearer challenge, or of all challenges
// when the header names no Bearer one.
func challengeParams(header string) map[string]string {
	var (
		bearer, all = map[string]string{}, map[string]string{}
		sawBearer   bool
		inBearer    bool
	)
	s := &challengeScanner{text: header}
	for {
		name, value, isParam, ok := s.next()
		if !ok {
			break
		}
		if !isParam {
			// A scheme opens the next challenge. Only the first Bearer challenge
			// is collected as such.
			inBearer = !sawBearer && strings.EqualFold(name, bearerScheme)
			sawBearer = sawBearer || inBearer
			continue
		}
		key := strings.ToLower(name)
		if _, stated := all[key]; !stated {
			all[key] = value
		}
		if _, stated := bearer[key]; inBearer && !stated {
			bearer[key] = value
		}
	}
	if sawBearer {
		return bearer
	}
	return all
}

// challengeScanner walks a WWW-Authenticate value one element at a time.
type challengeScanner struct {
	text string
	pos  int
}

// next returns the next element of the header: an auth-param (isParam, with its
// name and unescaped value) or a scheme name. ok is false once the header is
// exhausted or a quoted-string was left open, after which nothing that follows
// can be attributed to a parameter with any confidence.
func (s *challengeScanner) next() (name, value string, isParam, ok bool) {
	for s.pos < len(s.text) {
		s.skip(" \t,")
		if s.pos >= len(s.text) {
			break
		}
		name = s.token()
		if name == "" {
			// Not the start of a scheme or a parameter. A value with no name in
			// front of it is stepped over whole, so a quoted-string is never
			// entered halfway. The byte here is neither a separator nor a token
			// character, so either branch consumes it and the scan moves on.
			if s.text[s.pos] == '=' {
				s.pos++
				if _, closed := s.value(); !closed {
					return "", "", false, false
				}
				continue
			}
			if _, closed := s.value(); !closed {
				return "", "", false, false
			}
			continue
		}

		mark := s.pos
		s.skip(" \t")
		if s.pos >= len(s.text) || s.text[s.pos] != '=' {
			s.pos = mark
			return name, "", false, true
		}
		s.pos++
		s.skip(" \t")
		value, closed := s.value()
		if !closed {
			return "", "", false, false
		}
		return name, value, true, true
	}
	return "", "", false, false
}

// skip advances past any run of the given bytes.
func (s *challengeScanner) skip(set string) {
	for s.pos < len(s.text) && strings.IndexByte(set, s.text[s.pos]) >= 0 {
		s.pos++
	}
}

// token reads a run of token characters (RFC 9110 5.6.2), or "" when the next
// byte is not one.
func (s *challengeScanner) token() string {
	start := s.pos
	for s.pos < len(s.text) && isTokenChar(s.text[s.pos]) {
		s.pos++
	}
	return s.text[start:s.pos]
}

// value reads an auth-param value: a quoted-string with its escapes removed, or
// the run of bytes up to the next separator, which is empty when a separator
// comes first. closed is false for a quoted-string that never ends.
func (s *challengeScanner) value() (value string, closed bool) {
	if s.pos >= len(s.text) {
		return "", true
	}
	if s.text[s.pos] != '"' {
		start := s.pos
		for s.pos < len(s.text) && strings.IndexByte(" \t,", s.text[s.pos]) < 0 {
			s.pos++
		}
		return s.text[start:s.pos], true
	}

	s.pos++
	var b strings.Builder
	for s.pos < len(s.text) {
		ch := s.text[s.pos]
		s.pos++
		switch ch {
		case '"':
			return b.String(), true
		case '\\':
			if s.pos >= len(s.text) {
				return "", false
			}
			b.WriteByte(s.text[s.pos])
			s.pos++
		default:
			b.WriteByte(ch)
		}
	}
	return "", false
}

// isTokenChar reports whether ch is a tchar (RFC 9110 5.6.2): a letter, a digit,
// or one of the punctuation characters a token may hold.
func isTokenChar(ch byte) bool {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", ch) >= 0
	}
}
