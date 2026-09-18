package oauth

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/velocitykode/velocity/httpclient"
)

// Discovery resolves the authorization server for a protected MCP resource by
// fetching protected-resource metadata (RFC 9728) and then authorization-server
// metadata (RFC 8414 / OpenID Provider Metadata). Every URL it follows is
// advertised by the server being discovered, so each is required to be HTTPS
// (localhost excepted) and is fetched through a client that refuses, as the
// connection is opened, any host resolving to a private or internal address:
// a server cannot use discovery to reach into the network this application
// runs in, whatever name it gives the target.
type Discovery struct {
	// client carries the posture every resource gets, and loopback the one a
	// resource on the loopback interface gets instead (see hostPosture). With
	// allowPrivate set there is a single posture and loopback is unused.
	client   *httpclient.Client
	loopback *httpclient.Client
	// allowPrivate relaxes the HTTPS + non-internal-host requirements for
	// consumers that vouch for a private or plain-HTTP deployment (see
	// Config.AllowPrivateHosts). SSRF guards stay on by default.
	allowPrivate bool
}

// NewDiscovery builds a Discovery with the default OAuth endpoint client.
func NewDiscovery() *Discovery {
	return &Discovery{client: endpointClient(publicHosts), loopback: endpointClient(loopbackHosts)}
}

// NewDiscoveryAllowingPrivateHosts builds a Discovery that accepts plain-HTTP
// and private/internal endpoints (Config.AllowPrivateHosts semantics).
func NewDiscoveryAllowingPrivateHosts() *Discovery {
	return &Discovery{client: endpointClient(privateHosts), allowPrivate: true}
}

// clientFor returns the endpoint client for a discovery of resourceURL.
func (d *Discovery) clientFor(resourceURL string) *httpclient.Client {
	if postureFor(d.allowPrivate, resourceURL) == loopbackHosts {
		return d.loopback
	}
	return d.client
}

// requireSecureURL applies requireSecure unless private hosts are allowed.
func (d *Discovery) requireSecureURL(rawURL string) error {
	if d.allowPrivate {
		// Still insist the URL parses; only the transport posture is relaxed.
		u, err := url.Parse(rawURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return newError("unable to parse URL [%s] during OAuth discovery", rawURL)
		}
		return nil
	}
	return requireSecure(rawURL)
}

// requireExternalURL applies requireNotInternal unless private hosts are allowed.
func (d *Discovery) requireExternalURL(rawURL, resourceURL string) error {
	if d.allowPrivate {
		return nil
	}
	return requireNotInternal(rawURL, resourceURL)
}

// Discover resolves the authorization-server metadata for resourceURL. When
// resourceMetadataURL is non-empty it is treated as the explicit
// protected-resource metadata location (and a fetch failure is fatal);
// otherwise the well-known locations are derived from resourceURL and asked in
// the order the MCP authorization specification sets: the one built from the
// resource path, then the one at the root.
func (d *Discovery) Discover(ctx context.Context, resourceURL, resourceMetadataURL string) (*DiscoveryResult, error) {
	resourceURL = beforeFragment(resourceURL)
	client := d.clientFor(resourceURL)

	resourceMetadata, err := d.resourceMetadata(ctx, client, resourceURL, resourceMetadataURL)
	if err != nil {
		return nil, err
	}

	issuer := issuerFrom(resourceMetadata)
	if issuer == "" {
		issuer, err = origin(resourceURL)
		if err != nil {
			return nil, err
		}
	}

	if err := d.requireSecureURL(issuer); err != nil {
		return nil, err
	}
	if err := d.requireExternalURL(issuer, resourceURL); err != nil {
		return nil, err
	}

	serverMetadata, err := fetchMetadata(ctx, client, issuer)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare([]byte(issuer), []byte(serverMetadata.Issuer)) != 1 {
		return nil, newError("authorization server issuer [%s] did not match the expected issuer [%s]", serverMetadata.Issuer, issuer)
	}

	for _, endpoint := range []string{serverMetadata.AuthorizationEndpoint, serverMetadata.TokenEndpoint} {
		if err := d.requireSecureURL(endpoint); err != nil {
			return nil, err
		}
		if err := d.requireExternalURL(endpoint, resourceURL); err != nil {
			return nil, err
		}
	}
	if serverMetadata.RegistrationEndpoint != "" {
		if err := d.requireSecureURL(serverMetadata.RegistrationEndpoint); err != nil {
			return nil, err
		}
		if err := d.requireExternalURL(serverMetadata.RegistrationEndpoint, resourceURL); err != nil {
			return nil, err
		}
	}

	return &DiscoveryResult{
		Server:          serverMetadata,
		ScopesSupported: stringSlice(resourceMetadata, "scopes_supported"),
	}, nil
}

// metadataLocation is one place a protected-resource metadata document may be
// published. A document found there has to declare the resource being
// discovered, or one of the alternatives the location allows.
type metadataLocation struct {
	url          string
	alternatives []string
}

// metadataLocations lists where the metadata for resourceURL is looked for. A
// URL the resource advertised in its challenge is the only location when there
// is one. Otherwise the well-known URL built from the resource path comes
// first and the one at the root second, which is where a server whose MCP
// endpoint lives under a path may publish instead.
//
// RFC 9728 3.3 has the document declare the identifier its URL was derived
// from, and the root URL is derived from the origin alone. A document found
// there may therefore describe the origin as a whole as well as the resource
// itself; one found anywhere else has to name the resource exactly. Either way
// the token a flow ends in is requested for resourceURL and nothing else, so
// what a document declares never widens where that token is good.
func metadataLocations(resourceURL, advertisedURL string) ([]metadataLocation, error) {
	if advertisedURL != "" {
		return []metadataLocation{{url: advertisedURL}}, nil
	}
	inserted, err := wellKnown(resourceURL, "oauth-protected-resource")
	if err != nil {
		return nil, err
	}
	root, err := origin(resourceURL)
	if err != nil {
		return nil, err
	}
	locations := []metadataLocation{{url: inserted}}
	if atRoot := root + "/.well-known/oauth-protected-resource"; atRoot != inserted {
		locations = append(locations, metadataLocation{url: atRoot, alternatives: []string{root}})
	}
	return locations, nil
}

// resourceMetadata finds the protected-resource metadata document describing
// resourceURL. The first location that holds a document decides: the document
// is used, or discovery fails because it describes something else. Only a
// location that answers that it holds none moves discovery on to the next.
//
// When no location holds one the result is an empty document, which leaves the
// resource origin as the issuer: the arrangement of a server that publishes
// authorization-server metadata and no protected-resource metadata. That is
// reached only through answers, never through a failure to get one. A request
// that went unanswered says nothing about what the resource declares, and
// reading it as "declares nothing" would let an outage move a flow from the
// authorization server the resource names to a different one.
func (d *Discovery) resourceMetadata(ctx context.Context, client *httpclient.Client, resourceURL, advertisedURL string) (map[string]any, error) {
	locations, err := metadataLocations(resourceURL, advertisedURL)
	if err != nil {
		return nil, err
	}
	for _, location := range locations {
		if err := d.requireFetchable(location.url, resourceURL); err != nil {
			return nil, err
		}
		document, status, err := fetchResourceMetadata(ctx, client, location.url)
		if err != nil {
			return nil, err
		}
		if document == nil {
			// A location the resource advertised itself has no next one to try.
			if advertisedURL != "" {
				return nil, newError("protected resource metadata request to [%s] yielded no metadata document (status [%d])", location.url, status)
			}
			continue
		}
		if err := requireResourceMatches(document, resourceURL, location.alternatives...); err != nil {
			return nil, err
		}
		return document, nil
	}
	return map[string]any{}, nil
}

// fetchResourceMetadata asks one location for a protected-resource metadata
// document. It returns the document, or a nil document with the status when the
// location answered that it has none to give: any answer that is not a JSON
// object under a 2xx status (RFC 9728 3.2) and is not inconclusive. A request
// that failed, or was answered inconclusively, is an error.
func fetchResourceMetadata(ctx context.Context, client *httpclient.Client, metadataURL string) (map[string]any, int, error) {
	status, data, err := getJSON(ctx, client, metadataURL)
	if err != nil {
		return nil, 0, err
	}
	if inconclusive(status) {
		return nil, status, newError("protected resource metadata request to [%s] failed with status [%d]", metadataURL, status)
	}
	if !successful(status) || data == nil {
		return nil, status, nil
	}
	return data, status, nil
}

// inconclusive reports whether a status says the server could not answer this
// time, as opposed to answering that there is nothing to serve: a server error,
// a timeout, or a rate limit. The same request may well succeed later, so
// nothing can be concluded from it about what the server publishes.
func inconclusive(status int) bool {
	return status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// fetchMetadata tries the candidate well-known metadata URLs for issuer and
// returns the first that yields a valid metadata document.
func fetchMetadata(ctx context.Context, client *httpclient.Client, issuer string) (*AuthServerMetadata, error) {
	urls, err := metadataURLs(issuer)
	if err != nil {
		return nil, err
	}
	for _, metadataURL := range urls {
		status, data, err := getJSON(ctx, client, metadataURL)
		if err != nil || !successful(status) || data == nil {
			continue
		}
		return authServerMetadataFromMap(data)
	}
	return nil, newError("unable to discover authorization server metadata from [%s]", issuer)
}

// requireFetchable asserts a URL is HTTPS (or localhost) and not an internal host.
func (d *Discovery) requireFetchable(u, resourceURL string) error {
	if err := d.requireSecureURL(u); err != nil {
		return err
	}
	return d.requireExternalURL(u, resourceURL)
}

// requireResourceMatches asserts that the resource metadata declares one of the
// identifiers the document may describe, in practice the resource the metadata
// was requested for (RFC 9728 3.3). A document that declares none is refused
// like one that declares another: resource is a required member (RFC 9728 2),
// and without it nothing ties the authorization server the document names to
// the resource this client is about to send a token to.
//
// The comparison is exact but for the one difference RFC 3986 6.2.3 defines
// away: on http and https an empty path and "/" are the same URI, and a request
// line always carries at least a slash, so a server cannot tell which of the
// two a client used. A trailing slash anywhere else names a different resource
// and is not tolerated.
func requireResourceMatches(metadata map[string]any, resourceURL string, alternatives ...string) error {
	resource := stringField(metadata, "resource")
	if resource == "" {
		return newError("protected resource metadata does not declare the resource it describes")
	}
	declared := withoutEmptyPath(resource)
	matched := subtle.ConstantTimeCompare([]byte(withoutEmptyPath(resourceURL)), []byte(declared))
	for _, alternative := range alternatives {
		matched |= subtle.ConstantTimeCompare([]byte(withoutEmptyPath(alternative)), []byte(declared))
	}
	if matched != 1 {
		return newError("protected resource metadata resource [%s] did not match the expected resource [%s]", resource, resourceURL)
	}
	return nil
}

// withoutEmptyPath drops the path of an http(s) URL whose path is a bare slash,
// the empty-path equivalence of RFC 3986 6.2.3. Every other URL is returned as
// it stands.
//
// The URL is reassembled from its parsed components rather than trimmed as
// text, because the slash the equivalence covers is the whole path and a slash
// that ends a query or a fragment is content: dropping that one would equate
// two identifiers that differ.
//
// A URL that carries a fragment delimiter is left alone for the same reason. A
// fragment stated as empty parses to no fragment at all, so reassembling
// "https://host/#" would yield "https://host" and pass off an identifier that
// RFC 9728 2 forbids as the one that was asked about.
func withoutEmptyPath(rawURL string) string {
	if strings.Contains(rawURL, "#") {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Path != "/" || u.Host == "" {
		return rawURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return rawURL
	}
	u.Path = ""
	u.RawPath = ""
	return u.String()
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
// URL by inserting the well-known path between the host and the path of the
// resource identifier (RFC 9728 3.1).
//
// The path is carried over exactly as it stands, because a trailing slash names
// a different resource and dropping one asks the server to describe something
// else. The single exception is a bare slash, which is not a path a client can
// have meant differently (RFC 3986 6.2.3) and which would otherwise ask for a
// document at a different URL than an identifier with no path at all.
func wellKnown(rawURL, kind string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", newError("unable to parse URL [%s] during OAuth discovery", rawURL)
	}
	path := u.EscapedPath()
	if path == "/" {
		path = ""
	}
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

// requireNotInternal rejects a URL whose host is written as a loopback name or
// as a private or internal address, except when both the endpoint and the
// resource are themselves localhost.
//
// It reads the URL and nothing else, so it is not what keeps a flow out of the
// internal network: a name says nothing about the address it resolves to. That
// is settled where the connection is opened, by the guard of the endpoint
// client (see hostPosture). What this adds is an answer before any request is
// made, and the only answer there is for the authorization endpoint, which the
// user's browser is sent to and this client never connects to.
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
	return contains(loopbackNames, host)
}

// normalizedHost lowercases a host and strips any IPv6 brackets.
func normalizedHost(host string) string {
	return strings.ToLower(strings.Trim(host, "[]"))
}
