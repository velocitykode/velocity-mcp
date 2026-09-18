package oauth

import (
	"net"
	"net/url"
	"strings"
)

// reservedHostSuffixes are the special-use top-level domains that never resolve
// on the public internet (RFC 6761 / RFC 2606). An authorization server cannot
// fetch a client ID metadata document from one of them.
var reservedHostSuffixes = []string{".test", ".local", ".localhost", ".internal", ".invalid", ".example"}

// reservedIPBlocks are address ranges set aside for special purposes, so they
// never identify a host an authorization server could fetch a document from
// (RFC 6890 and the registries it points at). The standard library only
// classifies the loopback, link-local, unspecified and private ranges, so the
// remaining special-purpose blocks are listed here.
var reservedIPBlocks = parseCIDRs(
	"0.0.0.0/8",       // "this network"
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"240.0.0.0/4",     // reserved, including the limited broadcast address
	"64:ff9b:1::/48",  // local-use IPv4/IPv6 translation
	"100::/64",        // discard-only
	"2001::/23",       // IETF protocol assignments
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4
	"3fff::/20",       // documentation
	"5f00::/16",       // segment routing
)

// parseCIDRs parses static CIDR literals, skipping any that do not parse so a
// malformed entry cannot panic at package initialisation.
func parseCIDRs(blocks ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(blocks))
	for _, block := range blocks {
		if _, n, err := net.ParseCIDR(block); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

// clientIDMetadataURL returns the configured client ID metadata document URL
// when it can serve as this flow's client_id: the authorization server must
// advertise support for client ID metadata documents and the URL must itself be
// a usable public client identifier. It returns "" otherwise, which sends the
// caller on to dynamic client registration.
func (c *Client) clientIDMetadataURL(metadata *AuthServerMetadata) string {
	if !metadata.ClientIDMetadataDocumentSupported || c.config.ClientIDMetadataURL == "" {
		return ""
	}
	if !validClientIDMetadataURL(c.config.ClientIDMetadataURL) {
		return ""
	}
	return c.config.ClientIDMetadataURL
}

// validClientIDMetadataURL reports whether rawURL may be handed to an
// authorization server as a client_id: an HTTPS URL with a path component that
// carries no dot segments, no user info, query or fragment, and a host the
// server can resolve on the public internet. A URL that fails any of these is
// not a client identifier the server could dereference, so the flow falls back
// to dynamic registration instead of sending an identifier that would be
// rejected.
func validClientIDMetadataURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	if u.Path == "" || u.Path == "/" || hasDotSegment(u.Path) {
		return false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	// A fragment stated as empty is still a fragment, and the parsed Fragment
	// cannot tell it from none. The delimiter is what settles it: url.Parse
	// splits at the first "#", so one surviving in the raw text was written as
	// a fragment rather than carried inside the path.
	if strings.Contains(rawURL, "#") {
		return false
	}
	return publiclyResolvableHost(u.Hostname())
}

// hasDotSegment reports whether a decoded path carries a single-dot or
// double-dot segment. The identifier is compared as it is written, so a path
// that only resolves to the document after a server removes "." and ".."
// segments names a different string than the one the authorization server would
// dereference; the client identifier specification forbids both. The decoded
// path is what is inspected, so a percent-encoded spelling (%2e%2e) is caught
// alongside the literal one.
func hasDotSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// publiclyResolvableHost reports whether a host can be reached from outside this
// machine's network: not a loopback or reserved name, not a private, loopback,
// link-local, special-purpose or otherwise non-global address, not scoped to one
// interface by a zone identifier, and (for names) a fully qualified domain
// rather than a single intranet label or a non-canonical spelling of an address.
// A trailing root dot is stripped first, so "app.test." is judged as "app.test".
func publiclyResolvableHost(host string) bool {
	host = strings.TrimSuffix(normalizedHost(host), ".")
	if host == "" || isInternalHost(host) {
		return false
	}
	// A zone identifier (RFC 6874) scopes an address to one interface on one
	// machine, so it can never name a host another party could reach. It also
	// leaves a string that parses as neither an address nor a domain name, which
	// would otherwise fall through to the dotted-name check below.
	if strings.ContainsRune(host, '%') {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return publiclyRoutableIP(ip)
	}
	for _, suffix := range reservedHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	if numericTopLabel(host) {
		return false
	}
	return strings.Contains(host, ".")
}

// numericTopLabel reports whether a host's last label is all digits or a
// hexadecimal literal. No top-level domain has that form, so such a name is a
// non-canonical spelling of an IPv4 address (for example "127.1" or "0x7f.1")
// that the standard library declines to parse as an address, not a domain any
// authorization server could resolve.
func numericTopLabel(host string) bool {
	label := host[strings.LastIndexByte(host, '.')+1:]
	if rest, found := strings.CutPrefix(label, "0x"); found {
		return allInSet(rest, "0123456789abcdef")
	}
	return allInSet(label, "0123456789")
}

// allInSet reports whether s is non-empty and made only of bytes from set.
func allInSet(s, set string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !strings.ContainsRune(set, rune(s[i])) {
			return false
		}
	}
	return true
}

// publiclyRoutableIP reports whether an address is a global unicast address that
// no special-purpose registry has reserved.
func publiclyRoutableIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, block := range reservedIPBlocks {
		if block.Contains(ip) {
			return false
		}
	}
	return true
}
