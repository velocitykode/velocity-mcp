package oauth

// Token-endpoint client authentication methods (RFC 8414
// token_endpoint_auth_methods_supported / RFC 6749 client authentication).
const (
	authMethodNone  = "none"
	authMethodBasic = "client_secret_basic"
	authMethodPost  = "client_secret_post"
)

// AuthServerMetadata is the subset of authorization-server metadata (RFC 8414 /
// OpenID Provider Metadata) this client relies on.
type AuthServerMetadata struct {
	Issuer                                     string
	AuthorizationEndpoint                      string
	TokenEndpoint                              string
	RegistrationEndpoint                       string
	CodeChallengeMethodsSupported              []string
	AuthorizationResponseIssParameterSupported bool
	TokenEndpointAuthMethodsSupported          []string
}

// authServerMetadataFromMap parses a decoded metadata document, requiring the
// authorization and token endpoints to be present.
func authServerMetadataFromMap(data map[string]any) (*AuthServerMetadata, error) {
	authEndpoint := stringField(data, "authorization_endpoint")
	tokenEndpoint := stringField(data, "token_endpoint")
	if authEndpoint == "" || tokenEndpoint == "" {
		return nil, newError("authorization server metadata is missing required endpoints")
	}

	return &AuthServerMetadata{
		Issuer:                        stringField(data, "issuer"),
		AuthorizationEndpoint:         authEndpoint,
		TokenEndpoint:                 tokenEndpoint,
		RegistrationEndpoint:          stringField(data, "registration_endpoint"),
		CodeChallengeMethodsSupported: stringSlice(data, "code_challenge_methods_supported"),
		AuthorizationResponseIssParameterSupported: boolField(data, "authorization_response_iss_parameter_supported"),
		TokenEndpointAuthMethodsSupported:          stringSlice(data, "token_endpoint_auth_methods_supported"),
	}, nil
}

// DiscoveryResult bundles the resolved authorization-server metadata with the
// scopes the protected resource advertised as supported.
type DiscoveryResult struct {
	Server          *AuthServerMetadata
	ScopesSupported []string
}

// stringSlice returns the value at key coerced to a []string, or nil.
func stringSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// boolField returns the bool value at key, or false when absent or not a bool.
func boolField(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}
