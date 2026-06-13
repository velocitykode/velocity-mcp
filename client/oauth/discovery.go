package oauth

import (
	"context"
	"crypto/subtle"
	"net"
	"net/url"
	"strings"

	"github.com/velocitykode/velocity/httpclient"
)

// Discovery resolves the authorization server for a protected MCP resource by
// fetching protected-resource metadata (RFC 9728) and then authorization-server
// metadata (RFC 8414 / OpenID Provider Metadata). Every URL it follows is
// required to be HTTPS (localhost excepted) and to resolve to a non-internal
// host, guarding against server-advertised SSRF targets.
type Discovery struct {
	client *httpclient.Client
}

// NewDiscovery builds a Discovery with the default OAuth endpoint client.
func NewDiscovery() *Discovery {
	return &Discovery{client: endpointClient()}
}

// Discover resolves the authorization-server metadata for resourceURL. When
// resourceMetadataURL is non-empty it is treated as the explicit
// protected-resource metadata location (and a fetch failure is fatal);
// otherwise the well-known location is derived from resourceURL.
func (d *Discovery) Discover(ctx context.Context, resourceURL, resourceMetadataURL string) (*DiscoveryResult, error) {
	resourceURL = beforeFragment(resourceURL)

	metadataURL := resourceMetadataURL
	if metadataURL == "" {
		mu, err := wellKnown(resourceURL, "oauth-protected-resource")
		if err != nil {
			return nil, err
		}
		metadataURL = mu
	}

	if err := d.requireFetchable(metadataURL, resourceURL); err != nil {
		return nil, err
	}

	resourceMetadata, err := d.fetchResourceMetadata(ctx, metadataURL, resourceMetadataURL != "")
	if err != nil {
		return nil, err
	}

	if err := requireResourceMatches(resourceMetadata, resourceURL); err != nil {
		return nil, err
	}

	issuer := issuerFrom(resourceMetadata)
	if issuer == "" {
		issuer, err = origin(resourceURL)
		if err != nil {
			return nil, err
		}
	}

	if err := requireSecure(issuer); err != nil {
		return nil, err
	}
	if err := requireNotInternal(issuer, resourceURL); err != nil {
		return nil, err
	}

	serverMetadata, err := d.fetchMetadata(ctx, issuer)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare([]byte(issuer), []byte(serverMetadata.Issuer)) != 1 {
		return nil, newError("authorization server issuer [%s] did not match the expected issuer [%s]", serverMetadata.Issuer, issuer)
	}

	for _, endpoint := range []string{serverMetadata.AuthorizationEndpoint, serverMetadata.TokenEndpoint} {
		if err := requireSecure(endpoint); err != nil {
			return nil, err
		}
		if err := requireNotInternal(endpoint, resourceURL); err != nil {
			return nil, err
		}
	}
	if serverMetadata.RegistrationEndpoint != "" {
		if err := requireSecure(serverMetadata.RegistrationEndpoint); err != nil {
			return nil, err
		}
		if err := requireNotInternal(serverMetadata.RegistrationEndpoint, resourceURL); err != nil {
			return nil, err
		}
	}

	return &DiscoveryResult{
		Server:          serverMetadata,
		ScopesSupported: stringSlice(resourceMetadata, "scopes_supported"),
	}, nil
}

// fetchResourceMetadata fetches protected-resource metadata. When the fetch was
// not explicitly requested, failures degrade to an empty document so discovery
// can fall back to the resource origin as the issuer.
func (d *Discovery) fetchResourceMetadata(ctx context.Context, metadataURL string, explicit bool) (map[string]any, error) {
	status, data, err := getJSON(ctx, d.client, metadataURL)
	if err != nil {
		if explicit {
			return nil, err
		}
		return map[string]any{}, nil
	}
	if !successful(status) || data == nil {
		if explicit {
			return nil, newError("protected resource metadata request to [%s] failed with status [%d]", metadataURL, status)
		}
		return map[string]any{}, nil
	}
	return data, nil
}

// fetchMetadata tries the candidate well-known metadata URLs for issuer and
// returns the first that yields a valid metadata document.
func (d *Discovery) fetchMetadata(ctx context.Context, issuer string) (*AuthServerMetadata, error) {
	urls, err := metadataURLs(issuer)
	if err != nil {
		return nil, err
	}
	for _, metadataURL := range urls {
		status, data, err := getJSON(ctx, d.client, metadataURL)
		if err != nil || !successful(status) || data == nil {
			continue
		}
		return authServerMetadataFromMap(data)
	}
	return nil, newError("unable to discover authorization server metadata from [%s]", issuer)
}

// requireFetchable asserts a URL is HTTPS (or localhost) and not an internal host.
func (d *Discovery) requireFetchable(u, resourceURL string) error {
	if err := requireSecure(u); err != nil {
		return err
	}
	return requireNotInternal(u, resourceURL)
}

// requireResourceMatches asserts that, when the resource metadata declares a
// resource, it matches the expected resource URL.
func requireResourceMatches(metadata map[string]any, resourceURL string) error {
	resource := stringField(metadata, "resource")
	if resource != "" && subtle.ConstantTimeCompare([]byte(resourceURL), []byte(resource)) != 1 {
		return newError("protected resource metadata resource [%s] did not match the expected resource [%s]", resource, resourceURL)
	}
	return nil
}

// issuerFrom returns the first advertised authorization server, or "".
func issuerFrom(metadata map[string]any) string {
	servers := stringSlice(metadata, "authorization_servers")
	if len(servers) > 0 {
		return servers[0]
	}
	return ""
}

// metadataURLs returns the ordered well-known metadata candidates for an issuer,
// following the RFC 8414 / OpenID path-insertion rules.
func metadataURLs(issuer string) ([]string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, newError("unable to parse URL [%s] during OAuth discovery", issuer)
	}
	o := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")

	if path == "" {
		return []string{
			o + "/.well-known/oauth-authorization-server",
			o + "/.well-known/openid-configuration",
		}, nil
	}
	return []string{
		o + "/.well-known/oauth-authorization-server" + path,
		o + "/.well-known/openid-configuration" + path,
		o + path + "/.well-known/openid-configuration",
	}, nil
}

// wellKnown derives a well-known metadata URL of the given type from a resource
// URL, preserving the resource path component per RFC 9728.
func wellKnown(rawURL, kind string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", newError("unable to parse URL [%s] during OAuth discovery", rawURL)
	}
	path := strings.TrimSuffix(u.Path, "/")
	return u.Scheme + "://" + u.Host + "/.well-known/" + kind + path, nil
}

// origin returns the scheme://host[:port] origin of a URL.
func origin(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", newError("unable to parse URL [%s] during OAuth discovery", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// beforeFragment strips any URL fragment.
func beforeFragment(u string) string {
	if i := strings.IndexByte(u, '#'); i >= 0 {
		return u[:i]
	}
	return u
}

// requireSecure asserts a URL is served over HTTPS, allowing plaintext only for
// localhost during development.
func requireSecure(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return newError("unable to parse URL [%s] during OAuth discovery", rawURL)
	}
	if u.Scheme == "https" {
		return nil
	}
	if isLocalhost(normalizedHost(u.Hostname())) {
		return nil
	}
	return newError("OAuth endpoint [%s] must be served over HTTPS", rawURL)
}

// requireNotInternal rejects URLs that resolve to a private or internal host,
// except when both the endpoint and the resource are themselves localhost.
func requireNotInternal(rawURL, resourceURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return newError("unable to parse URL [%s] during OAuth discovery", rawURL)
	}
	r, err := url.Parse(resourceURL)
	if err != nil {
		return newError("unable to parse URL [%s] during OAuth discovery", resourceURL)
	}
	host := normalizedHost(u.Hostname())
	resourceHost := normalizedHost(r.Hostname())

	if isInternalHost(host) && !(isLocalhost(host) && isLocalhost(resourceHost)) {
		return newError("OAuth endpoint [%s] cannot use a private or internal host", rawURL)
	}
	return nil
}

// isInternalHost reports whether a host is localhost or a private/reserved IP.
func isInternalHost(host string) bool {
	if isLocalhost(host) {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// isLocalhost reports whether a host is a recognised loopback name or address.
func isLocalhost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// normalizedHost lowercases a host and strips any IPv6 brackets.
func normalizedHost(host string) string {
	return strings.ToLower(strings.Trim(host, "[]"))
}
