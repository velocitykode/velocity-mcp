package oauth

import (
	"context"
	"time"

	"github.com/velocitykode/velocity/httpclient"
)

// ClientRegistration is the result of dynamic client registration: the issued
// client identifier and optional secret.
type ClientRegistration struct {
	ClientID     string
	ClientSecret string
	// SecretExpiresAt is the moment the issued secret stops being accepted
	// (RFC 7591 client_secret_expires_at). It is the zero value when the
	// server issued no secret or declared one that never expires; a
	// registration that has passed it must not be reused.
	SecretExpiresAt time.Time
}

// Expired reports whether the issued secret has expired at now. A registration
// with no declared expiry never expires.
func (r *ClientRegistration) Expired(now time.Time) bool {
	return r != nil && !r.SecretExpiresAt.IsZero() && !now.Before(r.SecretExpiresAt)
}

// DefaultClientName is the client_name advertised during dynamic registration.
const DefaultClientName = "Velocity MCP Client"

// registerClient performs OAuth 2.0 Dynamic Client Registration (RFC 7591)
// against registrationEndpoint, requesting the authorization-code and
// refresh-token grants.
func registerClient(ctx context.Context, c *httpclient.Client, registrationEndpoint, redirectURI, scope, applicationType, tokenAuthMethod string) (*ClientRegistration, error) {
	payload := map[string]any{
		"client_name":                DefaultClientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": tokenAuthMethod,
		"application_type":           applicationType,
	}
	if scope != "" {
		payload["scope"] = scope
	}

	status, data, err := postJSON(ctx, c, registrationEndpoint, payload)
	if err != nil {
		return nil, err
	}
	if !successful(status) {
		return nil, newError("dynamic client registration failed with status [%d]", status)
	}

	clientID := stringField(data, "client_id")
	if clientID == "" {
		return nil, newError("dynamic client registration response did not include a client_id")
	}
	reg := &ClientRegistration{
		ClientID:     clientID,
		ClientSecret: stringField(data, "client_secret"),
	}
	// client_secret_expires_at is seconds since the epoch, with 0 meaning the
	// secret never expires. It is only meaningful alongside an issued secret.
	if secs, ok := intField(data, "client_secret_expires_at"); ok && secs > 0 && reg.ClientSecret != "" {
		reg.SecretExpiresAt = time.Unix(secs, 0)
	}
	return reg, nil
}
