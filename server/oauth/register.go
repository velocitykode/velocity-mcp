package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/velocitykode/velocity/router"
	"github.com/velocitykode/velocity/str"
	"github.com/velocitykode/velocity/validation"
)

// MaxRegistrationBodyBytes caps the registration request body. A registration
// document is a handful of short strings, so a tight bound costs nothing and
// keeps an anonymous, unauthenticated endpoint from being used to make the
// server buffer megabytes.
const MaxRegistrationBodyBytes int64 = 64 << 10 // 64 KiB

// maxClientNameLength and maxClientURLLength bound the free-text fields a
// client may register, and maxRedirectURIs how many redirect URIs it may claim,
// so a store is never handed unbounded strings or an unbounded list of them. A
// redirect URI is held to maxClientURLLength like the other URLs.
const (
	maxClientNameLength = 255
	maxClientURLLength  = 2048
	maxRedirectURIs     = 32
)

// tokenEndpointAuthNone is the client authentication method reported to
// dynamically registered clients: MCP clients are public clients that
// authenticate with PKCE rather than a client secret.
const tokenEndpointAuthNone = "none"

// fallbackClientName is used when a registration carries no name and no usable
// host to derive one from.
const fallbackClientName = "MCP Client"

// ClientRegistration is a validated dynamic client registration request
// (RFC 7591 2). Every field has already been checked against the Config's
// redirect policy, so a store may persist it as given.
type ClientRegistration struct {
	// Name is the client's display name, derived from client_name, name, or
	// the host of the first redirect URI. Never empty, never longer than 255
	// bytes, and made only of characters a person reading it can see: no
	// control characters (a NUL among them, which a text column may refuse),
	// no line breaks, and none of the invisible formatting characters that
	// reorder or hide text. It is still a name an anonymous caller chose, and a
	// consent screen that shows it says so.
	Name string
	// RedirectURIs are the client's redirect endpoints: at least one, at most
	// 32, none longer than 2048 bytes.
	RedirectURIs []string
	// LogoURI and ClientURI are the optional http(s) metadata URLs, or "",
	// neither longer than 2048 bytes. They are checked for shape and nothing
	// else. Nothing in this package fetches them, so which hosts they may name
	// is for whatever renders or dereferences them to decide: an application
	// that shows the logo on a consent screen, or fetches it to proxy it,
	// applies its own policy to a URL an anonymous caller supplied.
	LogoURI   string
	ClientURI string
	// Scope is the scope the resource requires, so a store that records
	// per-client scopes can grant it at creation.
	Scope string
}

// RegisteredClient is what a ClientStore returns after creating a client. Only
// ID is required; the empty fields fall back to what was registered and to the
// grants this server advertises.
type RegisteredClient struct {
	// ID is the issued client identifier. A store that returns an empty ID is
	// treated as having failed.
	ID string
	// Name is the display name actually recorded, when the store normalizes or
	// replaces the one it was handed. Empty means the registered name stands.
	Name string
	// RedirectURIs are the URIs actually recorded, when the store normalizes
	// or filters them. Empty means the registered ones stand.
	RedirectURIs []string
	// GrantTypes are the grants actually granted. Empty means the grants this
	// server advertises in its authorization-server metadata.
	GrantTypes []string
	// LogoURI and ClientURI are echoed back only when the store recorded them,
	// so a store that drops the metadata does not claim to have kept it.
	LogoURI   string
	ClientURI string
}

// ClientStore creates OAuth clients for dynamic registration. It is the one
// piece of dynamic registration this package cannot own: issuing a client
// identifier means persisting it wherever the application keeps its OAuth
// state. Implementations receive a request that is already validated against
// the Config's redirect policy.
//
// CreateClient must return a non-nil error rather than an empty RegisteredClient
// when it cannot create the client; the endpoint turns any error into a generic
// server_error response and never echoes it to the caller.
type ClientStore interface {
	CreateClient(ctx context.Context, registration ClientRegistration) (RegisteredClient, error)
}

// registrationResponse is the RFC 7591 3.2.1 client information response. It
// reports the metadata the client is registered under, which includes the
// values this server provisioned for it: a registration carries a name whether
// or not the client sent one, so client_name is always reported.
type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	RedirectURIs            []string `json:"redirect_uris"`
	Scope                   string   `json:"scope"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
}

// registrationError is the RFC 7591 3.2.2 error response.
type registrationError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Error codes defined by RFC 7591 3.2.2.
const (
	errInvalidRedirectURI    = "invalid_redirect_uri"
	errInvalidClientMetadata = "invalid_client_metadata"
	errServerError           = "server_error"
)

// registrationFields fixes the order metadata errors are reported in. Rule
// evaluation visits fields in map order, so which single failure a caller is
// told about is chosen here instead, keeping the response deterministic.
var registrationFields = []string{"client_name", "name", "logo_uri", "client_uri"}

// registerHandler serves dynamic client registration (RFC 7591): an
// unauthenticated POST through which a client that discovered this server
// provisions itself an identifier.
func (cfg Config) registerHandler() router.HandlerFunc {
	return func(c *router.Context) error {
		var payload map[string]any
		if err := c.Bind(&payload); err != nil || payload == nil {
			// A body that will not decode (malformed JSON, a JSON value that is
			// not an object, or one past the size cap) carries no metadata to report
			// on. The decoder's message is internal detail, so it is not echoed back.
			return writeRegistrationError(c, http.StatusBadRequest, errInvalidClientMetadata,
				"The registration request body could not be parsed.")
		}

		registration, failure := cfg.validateRegistration(payload)
		if failure != nil {
			return writeRegistrationError(c, http.StatusBadRequest, failure.Error, failure.ErrorDescription)
		}

		client, err := cfg.Clients.CreateClient(c.Request.Context(), registration)
		if err != nil || client.ID == "" {
			logRegistrationFailure(c, err)
			return writeRegistrationError(c, http.StatusInternalServerError, errServerError,
				"The client could not be registered.")
		}

		return c.JSON(http.StatusCreated, registrationResponse{
			ClientID:                client.ID,
			ClientName:              firstNonEmptyString(client.Name, registration.Name),
			GrantTypes:              firstNonEmpty(client.GrantTypes, []string{grantAuthorizationCode, grantRefreshToken}),
			ResponseTypes:           []string{responseTypeCode},
			RedirectURIs:            firstNonEmpty(client.RedirectURIs, registration.RedirectURIs),
			Scope:                   cfg.scope(),
			TokenEndpointAuthMethod: tokenEndpointAuthNone,
			LogoURI:                 client.LogoURI,
			ClientURI:               client.ClientURI,
		})
	}
}

// validateRegistration checks a decoded registration document against the
// Config's policy, returning the validated registration or the single error to
// report. It is exercised through the endpoint; the split keeps the policy
// testable without a response writer.
func (cfg Config) validateRegistration(payload map[string]any) (ClientRegistration, *registrationError) {
	rules := validation.Rules{
		"client_name":   {validation.Nullable(), validation.String(), validation.Max(maxClientNameLength), displayable},
		"name":          {validation.Nullable(), validation.String(), validation.Max(maxClientNameLength), displayable},
		"redirect_uris": {validation.Required(), validation.Array(), validation.Min(1), validation.Max(maxRedirectURIs)},
		"logo_uri":      {validation.Nullable(), validation.String(), validation.URL(), validation.Max(maxClientURLLength), displayable},
		"client_uri":    {validation.Nullable(), validation.String(), validation.URL(), validation.Max(maxClientURLLength), displayable},
	}

	validated, err := validation.NewValidator().Validate(payload, rules)
	verr := fieldErrors(validated, err)

	// The redirect policy is reported first and as its own error code: a client
	// that cannot register its callback has nothing else worth fixing first.
	// The shape of the array comes from the engine, the policy on each element
	// is checked here, because the rule engine addresses fields, not elements.
	if message := verr.First("redirect_uris"); message != "" {
		return ClientRegistration{}, &registrationError{Error: errInvalidRedirectURI, ErrorDescription: message}
	}
	if message := cfg.checkRedirectURIs(payload["redirect_uris"]); message != "" {
		return ClientRegistration{}, &registrationError{Error: errInvalidRedirectURI, ErrorDescription: message}
	}
	if err != nil {
		return ClientRegistration{}, metadataFailure(verr)
	}

	uris := stringSlice(payload["redirect_uris"])
	return ClientRegistration{
		Name:         clientName(payload, uris),
		RedirectURIs: uris,
		LogoURI:      stringValue(payload, "logo_uri"),
		ClientURI:    stringValue(payload, "client_uri"),
		Scope:        cfg.scope(),
	}, nil
}

// displayable is the rule the text a client registers has to pass: every
// character in it is one a person reading it can see, or the space between two
// of them. The name is shown to the user who is asked to trust the client, on a
// consent screen and wherever else the application lists its clients, and so
// are its URLs. All of it is supplied by an anonymous caller, so what is stored
// has to be what is seen.
var displayable = validation.Custom("displayable", func(field string, value interface{}, _ []string, _ map[string]interface{}) error {
	text, ok := value.(string)
	if !ok {
		// The string rule has already reported a value of another type.
		return nil
	}
	if !isDisplayableText(text) {
		return fieldMessage("The " + field + " field must not contain control or invisible formatting characters.")
	}
	return nil
})

// fieldMessage is what a rule of this package reports for a field that failed
// it. The validation engine takes that report as an error, but it is message
// text and not a Go error string: it ends up in the RFC 7591 error_description
// a client shows, so it is written as the full sentence the engine's own rules
// use.
type fieldMessage string

// Error implements the error interface.
func (m fieldMessage) Error() string { return string(m) }

// isDisplayableText reports whether every character of text is displayable.
func isDisplayableText(text string) bool {
	for _, r := range text {
		if !isDisplayable(r) {
			return false
		}
	}
	return true
}

// isDisplayable reports whether r may appear in registered text. Refused are the
// control characters (NUL, CR and LF among them), the line and paragraph
// separators, and the format characters: the bidirectional controls that make a
// name read differently from how it is stored, the zero-width and tag
// characters that hide text inside it, and the rest of that category. The two
// joiners are the exception, because scripts and emoji sequences need them to
// render the visible characters around them correctly.
func isDisplayable(r rune) bool {
	const zeroWidthNonJoiner, zeroWidthJoiner = '\u200c', '\u200d'
	if r == zeroWidthNonJoiner || r == zeroWidthJoiner {
		return true
	}
	return !unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// fieldErrors recovers the per-field messages from a validation run. The engine
// reports a rule set it cannot run at all without a ValidatedData; the rule set
// here is a constant, so that path only guards against a future edit.
func fieldErrors(validated *validation.ValidatedData, err error) validation.ValidationErrors {
	if validated != nil {
		return validated.Errors()
	}
	var verr validation.ValidationErrors
	_ = errors.As(err, &verr)
	return verr
}

// metadataFailure reduces the remaining field errors to the single
// invalid_client_metadata answer, choosing which field to report in a fixed
// order because the engine visits fields in map order.
func metadataFailure(verr validation.ValidationErrors) *registrationError {
	for _, field := range registrationFields {
		if message := verr.First(field); message != "" {
			return &registrationError{Error: errInvalidClientMetadata, ErrorDescription: message}
		}
	}
	return &registrationError{Error: errInvalidClientMetadata, ErrorDescription: "The client metadata was invalid."}
}

// checkRedirectURIs applies the redirect policy to every URI a registration
// claims, returning the message for the first that fails or "" when they all
// pass. The message names the offending index so a client can find it.
//
// This is not a validation rule because the rule engine addresses fields, not
// array elements: a rule on "redirect_uris" sees the whole array and could only
// report one message for it anyway.
func (cfg Config) checkRedirectURIs(value any) string {
	items, ok := value.([]any)
	if !ok {
		// The array rule has already reported a non-array value.
		return ""
	}
	for i, item := range items {
		name := fmt.Sprintf("redirect_uris.%d", i)
		uri, ok := item.(string)
		if !ok || uri == "" {
			return name + notAValidURL
		}
		if len(uri) > maxClientURLLength {
			return fmt.Sprintf("%s must not exceed %d characters.", name, maxClientURLLength)
		}
		if message := cfg.checkRedirectURI(name, uri); message != "" {
			return message
		}
	}
	return ""
}

// Client-facing endings for the redirect policy messages. They are message
// text, not Go error strings: the endpoint puts them in the RFC 7591
// error_description field, where a full sentence is what a client shows.
const (
	notAValidURL       = " is not a valid URL."
	notPermittedDomain = " is not a permitted redirect domain."
	notSecureTransport = " must use https, or http only on a loopback address."
	hasDotSegments     = " must not contain \".\" or \"..\" path segments."
)

// checkRedirectURI applies the redirect policy to one URI: it must be a
// syntactically valid absolute URI with no fragment, and either an http(s) URL
// that is transport-secure and whose origin the application allows, or a
// private-use scheme (RFC 8252) the application lists. It returns the message
// to report, or "" when the URI is permitted.
func (cfg Config) checkRedirectURI(name, uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme == "" {
		return name + notAValidURL
	}
	// A URI has no raw control or formatting characters in it (RFC 3986 2), and
	// one that did would not read on a consent screen the way it is stored, nor
	// would the display name derived from its host.
	if !isDisplayableText(uri) {
		return name + notAValidURL
	}
	// A redirection endpoint URI must not carry a fragment (RFC 6749 3.1.2,
	// kept by OAuth 2.1): the fragment is where the authorization server puts
	// its own response, so one registered here has nowhere to survive.
	if parsed.Fragment != "" || parsed.RawFragment != "" || strings.HasSuffix(uri, "#") {
		return name + notAValidURL
	}
	// A redirection endpoint names where the authorization response is
	// delivered; userinfo names a credential instead (RFC 3986 3.2.1), and it
	// is where a string that only looks like a host hides, since everything
	// before the "@" belongs to the credential and not to the authority the
	// response actually travels to. No scheme has a use for it here.
	if parsed.User != nil {
		return name + notAValidURL
	}
	// A redirect URI is compared as it is written, here against the allowlist
	// and later by the authorization server against the one a request names, so
	// a path that only says where it leads once its dot segments are resolved
	// (RFC 3986 5.2.4) is never what a client needs. It is what an entry scoped
	// to a path is escaped with: https://example.com/app/../../elsewhere starts
	// with that entry and is delivered outside it.
	if hasDotSegment(parsed.Path) {
		return name + hasDotSegments
	}
	scheme := strings.ToLower(parsed.Scheme)

	if scheme != "http" && scheme != "https" {
		// A private-use scheme carries no authority a domain list could
		// constrain, so the scheme allowlist is the whole check. Schemes are
		// case-insensitive (RFC 3986 3.1), so the configured list is folded
		// too rather than demanding the client spell it the same way.
		if !containsFold(cfg.CustomSchemes, scheme) {
			return name + notAValidURL
		}
		return privateUseTarget(name, parsed)
	}

	if parsed.Hostname() == "" {
		// An authority that carries only a port, or none at all, names no
		// server the authorization response could be delivered to.
		return name + notAValidURL
	}
	// The authorization response travels back over the redirect URI, so a
	// callback that is not on the loopback interface has to be encrypted: a
	// plaintext hop hands the authorization code to anyone on the path. That is
	// a transport requirement, not a question of which domains the application
	// trusts, so it is applied ahead of the allowlist and a wildcard does not
	// lift it.
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return name + notSecureTransport
	}
	if contains(cfg.RedirectDomains, "*") {
		return ""
	}
	if cfg.allowsLoopback() && isLoopbackURL(uri) {
		return ""
	}
	if !str.StartsWith(uri, cfg.allowedOrigins()...) {
		return name + notPermittedDomain
	}
	return ""
}

// privateUseTarget checks that a redirect URI on an allowed private-use scheme
// names something for the authorization response to be delivered to, returning
// the message to report or "" when it does.
//
// RFC 8252 7.1 writes such a callback as scheme:/path, with no authority
// component at all, so a missing host is not what makes one invalid: a native
// client registering com.example.app:/oauth2redirect/provider is following the
// specification. The operating system routes such a URI on its scheme alone, so
// what has to be there is a target the client can be handed: a host, or a path.
// What is rejected is a URI that names neither, a rootless target
// (scheme:target) that RFC 8252 does not define for this use, and an authority
// made of nothing but a port, which names no destination on any scheme.
func privateUseTarget(name string, parsed *url.URL) string {
	switch {
	case parsed.Opaque != "", parsed.Host != "" && parsed.Hostname() == "":
		return name + notAValidURL
	case parsed.Hostname() != "", parsed.Path != "":
		return ""
	default:
		return name + notAValidURL
	}
}

// allowedOrigins returns the configured redirect domains each terminated with a
// slash, so "https://example.com" matches https://example.com/callback but not
// https://example.com.attacker.test/callback.
func (cfg Config) allowedOrigins() []string {
	origins := make([]string, 0, len(cfg.RedirectDomains))
	for _, domain := range cfg.RedirectDomains {
		if domain == "" || domain == "*" {
			continue
		}
		if !str.EndsWith(domain, "/") {
			domain += "/"
		}
		origins = append(origins, domain)
	}
	return origins
}

// loopbackHosts are the hosts a native client may use for the RFC 8252 loopback
// redirect, on which the port is chosen at runtime and cannot be registered.
// They are bare hosts: the IPv6 literal is compared without its brackets.
var loopbackHosts = []string{"localhost", "127.0.0.1", "::1"}

// allowsLoopback reports whether the configured redirect domains include a
// loopback host, which is what lets a native client register
// http://127.0.0.1:<any port>/callback.
//
// The allowance is for plain HTTP, so only an entry that could mean plain HTTP
// grants it: one written with the http scheme, or with no scheme at all. An
// entry such as https://localhost says something narrower, and is honoured as
// the origin prefix it is rather than read as leave to use an unencrypted
// callback the application never listed.
func (cfg Config) allowsLoopback() bool {
	for _, domain := range cfg.RedirectDomains {
		if str.Contains(domain, "://") && !strings.EqualFold(str.Before(domain, "://"), "http") {
			continue
		}
		if isLoopbackHost(strings.TrimRight(str.After(domain, "://"), "/")) {
			return true
		}
	}
	return false
}

// hasDotSegment reports whether a decoded path carries a "." or ".." segment.
// The decoded path is what is inspected, so a percent-encoded spelling (%2e%2e)
// is caught alongside the literal one, and a backslash separates segments as a
// slash does, because that is how a browser reads one in an http(s) URL.
func hasDotSegment(path string) bool {
	segments := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	for _, segment := range segments {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// isLoopbackURL reports whether uri is a plain-HTTP loopback redirect.
func isLoopbackURL(uri string) bool {
	parsed, err := url.Parse(uri)
	if err != nil || strings.ToLower(parsed.Scheme) != "http" {
		return false
	}
	return isLoopbackHost(parsed.Hostname())
}

// isLoopbackHost reports whether host is one of the loopback names RFC 8252
// names for a native client's redirect. The comparison is exact, on the host
// alone, so "localhost.attacker.test" does not pass as loopback and a port does
// not change the answer.
func isLoopbackHost(host string) bool {
	return contains(loopbackHosts, strings.ToLower(strings.Trim(host, "[]")))
}

// clientName resolves the display name for a registration: the client_name it
// supplied, then the name alias, then the host of its first redirect URI, then
// a generic fallback.
func clientName(payload map[string]any, uris []string) string {
	if name := stringValue(payload, "client_name"); name != "" {
		return name
	}
	if name := stringValue(payload, "name"); name != "" {
		return name
	}
	if len(uris) > 0 {
		if parsed, err := url.Parse(uris[0]); err == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
	}
	return fallbackClientName
}

// stringValue reads a string field from a decoded document, or "".
func stringValue(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}

// stringSlice coerces a decoded JSON array into a []string. Non-string elements
// are skipped, which validation has already rejected the document for by the
// time this runs.
func stringSlice(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// contains reports whether values holds needle.
func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}

// containsFold reports whether values holds needle, ignoring ASCII case.
func containsFold(values []string, needle string) bool {
	for _, v := range values {
		if strings.EqualFold(v, needle) {
			return true
		}
	}
	return false
}

// firstNonEmpty returns primary when it has elements, otherwise fallback.
func firstNonEmpty(primary, fallback []string) []string {
	if len(primary) > 0 {
		return primary
	}
	return fallback
}

// firstNonEmptyString returns primary when it is set, otherwise fallback.
func firstNonEmptyString(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

// writeRegistrationError writes an RFC 7591 error response. Descriptions are
// always this package's own wording, never a wrapped internal error.
func writeRegistrationError(c *router.Context, status int, code, description string) error {
	return c.JSON(status, registrationError{Error: code, ErrorDescription: description})
}

// logRegistrationFailure records why a store refused to create a client. The
// detail stays server-side: the caller gets the generic server_error response.
func logRegistrationFailure(c *router.Context, err error) {
	s := c.ServicesIfSet()
	if s == nil || s.Log == nil {
		return
	}
	if err == nil {
		s.Log.Error("mcp oauth client registration failed", "error", "the client store returned no client id")
		return
	}
	s.Log.Error("mcp oauth client registration failed", "error", err.Error())
}
