package ui

// Csp is the Content Security Policy an app resource requests: the domain
// allowlists the host applies to the sandboxed frame. Each field maps to a CSP
// directive; an unset (empty) list is omitted, so the host applies its own
// default for that directive.
type Csp struct {
	connectDomains  []string
	resourceDomains []string
	frameDomains    []string
	baseURIDomains  []string
}

// NewCsp builds an empty Content Security Policy.
func NewCsp() Csp { return Csp{} }

// WithConnectDomains sets the domains the app may reach via fetch, XHR, or
// WebSocket (the connect-src directive).
func (c Csp) WithConnectDomains(domains ...string) Csp {
	c.connectDomains = domains
	return c
}

// WithResourceDomains sets the domains the app may load images, scripts,
// styles, and fonts from (the default-src directive).
func (c Csp) WithResourceDomains(domains ...string) Csp {
	c.resourceDomains = domains
	return c
}

// WithFrameDomains sets the domains the app may embed as nested frames (the
// frame-src directive).
func (c Csp) WithFrameDomains(domains ...string) Csp {
	c.frameDomains = domains
	return c
}

// WithBaseURIDomains sets the allowed URLs for the document base element (the
// base-uri directive).
func (c Csp) WithBaseURIDomains(domains ...string) Csp {
	c.baseURIDomains = domains
	return c
}

// withMergedResourceDomains returns a copy with extra resource domains appended,
// de-duplicated and order-preserving. Used to fold in the CDN hosts required by
// the app's libraries.
func (c Csp) withMergedResourceDomains(extra []string) Csp {
	if len(extra) == 0 {
		return c
	}
	seen := make(map[string]struct{}, len(c.resourceDomains)+len(extra))
	merged := make([]string, 0, len(c.resourceDomains)+len(extra))
	for _, d := range append(append([]string(nil), c.resourceDomains...), extra...) {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		merged = append(merged, d)
	}
	c.resourceDomains = merged
	return c
}

// isEmpty reports whether no directive is set.
func (c Csp) isEmpty() bool {
	return len(c.connectDomains) == 0 && len(c.resourceDomains) == 0 &&
		len(c.frameDomains) == 0 && len(c.baseURIDomains) == 0
}

// ToMap renders the policy to its wire shape, omitting any unset directive.
func (c Csp) ToMap() map[string]any {
	m := map[string]any{}
	if len(c.connectDomains) > 0 {
		m["connectDomains"] = c.connectDomains
	}
	if len(c.resourceDomains) > 0 {
		m["resourceDomains"] = c.resourceDomains
	}
	if len(c.frameDomains) > 0 {
		m["frameDomains"] = c.frameDomains
	}
	if len(c.baseURIDomains) > 0 {
		m["baseUriDomains"] = c.baseURIDomains
	}
	return m
}
