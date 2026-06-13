package oauth

import (
	"regexp"
	"strings"
)

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

// challengeParam matches a single auth-param of the form key=value, where the
// value is either a quoted string or an unquoted token.
var challengeParam = regexp.MustCompile(`([\w-]+)\s*=\s*("[^"]*"|[^,\s]+)`)

// ParseChallenge parses a WWW-Authenticate header value into a Challenge.
// A missing or empty header yields a zero-value Challenge (no error), matching
// servers that decline to advertise discovery metadata.
func ParseChallenge(header string) *Challenge {
	c := &Challenge{}
	if strings.TrimSpace(header) == "" {
		return c
	}

	for _, m := range challengeParam.FindAllStringSubmatch(header, -1) {
		key := strings.ToLower(m[1])
		value := unquote(m[2])

		switch key {
		case "resource_metadata":
			c.ResourceMetadataURL = value
		case "error":
			c.Error = value
		case "error_description":
			c.ErrorDescription = value
		case "scope":
			c.Scope = value
		}
	}
	return c
}

// unquote strips a single pair of surrounding double quotes from an auth-param
// value, leaving unquoted tokens untouched.
func unquote(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}
