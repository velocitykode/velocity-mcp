package oauth

import (
	"strings"
	"time"
)

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
	// Issuer is the authorization server that issued this set. A refresh token
	// and the client credentials that obtained it are only ever meaningful to
	// that server, so Refresh presents the set to no other one. An application
	// that persists a TokenSet has to persist Issuer with it; a set that records
	// none can no longer be refreshed.
	Issuer string
}

// Expired reports whether the token has an expiry that is at or before now.
// A token with no expiry (zero ExpiresAt) is never reported as expired.
func (t TokenSet) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// tokenTypeBearer is the one access token type this client knows how to
// present: in the Authorization header, as RFC 6750 2.1 describes and the MCP
// authorization specification requires.
const tokenTypeBearer = "Bearer"

// requireBearerTokenType refuses a token response whose token_type is anything
// but Bearer. A client must not use an access token of a type it does not
// understand (RFC 6749 7.1): a type such as mac or DPoP says the token is only
// good together with a proof this client cannot produce, and sending it as a
// bearer token anyway would present it without the binding it was issued under.
//
// The type is compared without regard to case (RFC 6749 5.1). A response that
// omits the member altogether is read as Bearer, the only type an MCP
// authorization server issues; one that states it as anything other than a
// string has stated a type this client does not understand.
func requireBearerTokenType(data map[string]any) error {
	value, stated := data["token_type"]
	if !stated {
		return nil
	}
	tokenType, ok := value.(string)
	if !ok {
		return newError("the token response states a token_type that is not a string")
	}
	if !strings.EqualFold(tokenType, tokenTypeBearer) {
		return newError("the token response is of a token_type other than Bearer, which this client cannot present")
	}
	return nil
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
		t.TokenType = tokenTypeBearer
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
// and Go integers, along with whether a usable value was found. A value of any
// other type, including a numeric string, is not usable.
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
