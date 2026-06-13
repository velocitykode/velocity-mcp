package oauth

import (
	"context"

	"github.com/velocitykode/velocity/httpclient"
)

// ClientRegistration is the result of dynamic client registration: the issued
// client identifier and optional secret.
type ClientRegistration struct {
	ClientID     string
	ClientSecret string
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
	return &ClientRegistration{
		ClientID:     clientID,
		ClientSecret: stringField(data, "client_secret"),
	}, nil
}
