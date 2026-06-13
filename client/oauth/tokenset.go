package oauth

import "time"

// TokenSet is the result of a successful token request: the access token plus
// the optional refresh token, expiry, scope, and the client credentials used to
// obtain it (so a later refresh can reuse them).
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is the absolute expiry time, or the zero value when the server
	// did not return expires_in.
	ExpiresAt    time.Time
	TokenType    string
	Scope        string
	ClientID     string
	ClientSecret string
}

// Expired reports whether the token has an expiry that is at or before now.
// A token with no expiry (zero ExpiresAt) is never reported as expired.
func (t TokenSet) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// tokenSetFromResponse builds a TokenSet from a decoded token-endpoint JSON
// body, anchoring any relative expires_in to now.
func tokenSetFromResponse(data map[string]any, now time.Time) TokenSet {
	t := TokenSet{
		AccessToken:  stringField(data, "access_token"),
		RefreshToken: stringField(data, "refresh_token"),
		TokenType:    stringField(data, "token_type"),
		Scope:        stringField(data, "scope"),
	}
	if t.TokenType == "" {
		t.TokenType = "Bearer"
	}
	if secs, ok := intField(data, "expires_in"); ok {
		t.ExpiresAt = now.Add(time.Duration(secs) * time.Second)
	}
	return t
}

// stringField returns the string value at key, coercing common JSON scalar
// types, or "" when absent or of another type.
func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// intField returns the integer value at key, accepting JSON numbers (float64)
// and numeric strings, along with whether a usable value was found.
func intField(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}
