package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE holds a Proof Key for Code Exchange pair (RFC 7636): the high-entropy
// verifier kept by the client and the S256 challenge sent on the authorization
// request.
type PKCE struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE produces a fresh PKCE pair using 64 random bytes for the verifier
// and its SHA-256 digest (the only code-challenge method MCP requires) for the
// challenge, both base64url-encoded without padding.
func GeneratePKCE() (PKCE, error) {
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return PKCE{}, wrapError(err, "unable to generate PKCE verifier")
	}

	verifier := base64URL(raw)
	sum := sha256.Sum256([]byte(verifier))

	return PKCE{Verifier: verifier, Challenge: base64URL(sum[:])}, nil
}

// base64URL encodes bytes using the URL-safe alphabet without padding.
func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
