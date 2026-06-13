package ui

import "strings"

// AppMeta is the top-level metadata for an MCP UI app resource, emitted under
// the "_meta.ui" key of the resource's resources/list entry. It bundles the
// Content Security Policy, requested browser permissions, the app's domain, a
// border-preference hint, and the front-end libraries to load.
//
// The zero value is usable (an empty app); NewAppMeta seeds the conventional
// default of prefersBorder=true. AppMeta is an immutable value: each With method
// returns an updated copy.
type AppMeta struct {
	csp           Csp
	hasCsp        bool
	permissions   Permissions
	domain        string
	prefersBorder *bool
	libraries     []Library
}

// NewAppMeta builds app metadata with the conventional prefersBorder=true
// default. Set other fields with the With methods.
func NewAppMeta() AppMeta {
	border := true
	return AppMeta{prefersBorder: &border}
}

// WithCsp sets the Content Security Policy the host applies to the app frame.
func (m AppMeta) WithCsp(csp Csp) AppMeta {
	m.csp = csp
	m.hasCsp = true
	return m
}

// WithPermissions sets the browser capabilities the app requests.
func (m AppMeta) WithPermissions(p Permissions) AppMeta {
	m.permissions = p
	return m
}

// WithDomain sets the app's domain, used by the host to scope and label the
// app frame.
func (m AppMeta) WithDomain(domain string) AppMeta {
	m.domain = domain
	return m
}

// WithPrefersBorder sets whether the host should render a border around the app
// frame.
func (m AppMeta) WithPrefersBorder(prefers bool) AppMeta {
	m.prefersBorder = &prefers
	return m
}

// WithLibraries sets the front-end libraries to load into the app. Their script
// tags are injected into the served HTML and their CDN hosts merged into the
// CSP resourceDomains.
func (m AppMeta) WithLibraries(libs ...Library) AppMeta {
	m.libraries = libs
	return m
}

// Libraries returns the configured libraries.
func (m AppMeta) Libraries() []Library {
	return m.libraries
}

// ScriptTags returns the combined HTML script tags for all configured
// libraries, newline-joined and ready to inject into the document head. It is
// empty when no library is configured.
func (m AppMeta) ScriptTags() string {
	var tags []string
	for _, lib := range m.libraries {
		tags = append(tags, lib.ScriptTags()...)
	}
	return strings.Join(tags, "\n")
}

// libraryDomains returns the CSP resource domains required by all configured
// libraries.
func (m AppMeta) libraryDomains() []string {
	var out []string
	for _, lib := range m.libraries {
		out = append(out, lib.Domains()...)
	}
	return out
}

// IsEmpty reports whether the metadata carries nothing worth emitting. A
// prefers-border value alone counts as content (it is a deliberate hint).
func (m AppMeta) IsEmpty() bool {
	return len(m.ToMap()) == 0
}

// ToMap renders the metadata to its "_meta.ui" wire shape, omitting unset
// fields. The configured libraries' CDN hosts are merged into the CSP
// resourceDomains so the document can load them under the policy.
func (m AppMeta) ToMap() map[string]any {
	out := map[string]any{}

	csp := m.csp
	csp = csp.withMergedResourceDomains(m.libraryDomains())
	if !csp.isEmpty() {
		out["csp"] = csp.ToMap()
	}

	if !m.permissions.isEmpty() {
		out["permissions"] = m.permissions.ToMap()
	}
	if m.domain != "" {
		out["domain"] = m.domain
	}
	if m.prefersBorder != nil {
		out["prefersBorder"] = *m.prefersBorder
	}
	return out
}
