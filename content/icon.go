package content

// Icon is an icon reference attached to a ResourceLink. It mirrors the
// laravel/mcp Schema\Icon wire shape. Only non-empty fields appear in the
// marshaled output.
//
// Icon is declared here (rather than imported from the schema package) so that
// content remains a stdlib-only leaf with no sibling-package dependency. The
// schema package's Icon, when it lands, carries identical wire semantics.
type Icon struct {
	// Src is the icon source (URL or asset path). Always emitted.
	Src string
	// MimeType is the optional icon mime type. Omitted when empty.
	MimeType string
	// Sizes is the optional list of size descriptors (e.g. "48x48").
	// Omitted when empty.
	Sizes []string
	// Theme is the optional theme hint (e.g. "light" or "dark").
	// Omitted when empty.
	Theme string
}

// toArray returns the icon wire shape, dropping empty optional fields.
func (ic Icon) toArray() map[string]any {
	out := map[string]any{"src": ic.Src}
	if ic.MimeType != "" {
		out["mimeType"] = ic.MimeType
	}
	if len(ic.Sizes) > 0 {
		sizes := make([]string, len(ic.Sizes))
		copy(sizes, ic.Sizes)
		out["sizes"] = sizes
	}
	if ic.Theme != "" {
		out["theme"] = ic.Theme
	}
	return out
}
