package schema

import "encoding/json"

// IconTheme is the optional theme hint for an icon, mirroring laravel/mcp's
// IconTheme enum.
type IconTheme string

const (
	// IconThemeLight indicates the icon is intended for light backgrounds.
	IconThemeLight IconTheme = "light"
	// IconThemeDark indicates the icon is intended for dark backgrounds.
	IconThemeDark IconTheme = "dark"
)

// Valid reports whether the theme is one of the known values.
func (t IconTheme) Valid() bool {
	switch t {
	case IconThemeLight, IconThemeDark:
		return true
	default:
		return false
	}
}

// Icon is protocol metadata describing an icon associated with an
// implementation or primitive, mirroring laravel/mcp's Schema\Icon. It marshals
// to its MCP wire shape, omitting any unset optional field:
//
//	{"src":"...","mimeType":"...","sizes":["48x48"],"theme":"dark"}
type Icon struct {
	// Src is the icon source (URI or path). Required.
	Src string
	// MimeType is the optional MIME type of the icon.
	MimeType string
	// Sizes is the optional list of size descriptors (e.g. "48x48").
	Sizes []string
	// Theme is the optional theme hint.
	Theme IconTheme
}

// NewIcon constructs an Icon from its source. Optional fields are set via the
// returned value's exported fields or the With* helpers.
func NewIcon(src string) Icon {
	return Icon{Src: src}
}

// WithMimeType returns a copy of the icon with the given MIME type.
func (i Icon) WithMimeType(mimeType string) Icon {
	i.MimeType = mimeType
	return i
}

// WithSizes returns a copy of the icon with the given size descriptors.
func (i Icon) WithSizes(sizes ...string) Icon {
	i.Sizes = append([]string(nil), sizes...)
	return i
}

// WithTheme returns a copy of the icon with the given theme.
func (i Icon) WithTheme(theme IconTheme) Icon {
	i.Theme = theme
	return i
}

// ToMap renders the icon as a map, omitting unset optional fields, mirroring
// laravel's Arr::whereNotNull behavior.
func (i Icon) ToMap() map[string]any {
	m := map[string]any{
		"src": i.Src,
	}
	if i.MimeType != "" {
		m["mimeType"] = i.MimeType
	}
	if len(i.Sizes) > 0 {
		m["sizes"] = i.Sizes
	}
	if i.Theme != "" {
		m["theme"] = string(i.Theme)
	}
	return m
}

// MarshalJSON implements json.Marshaler.
func (i Icon) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.ToMap())
}

// Implementation is protocol metadata describing an MCP implementation
// (server or client), mirroring laravel/mcp's Schema\Implementation. It
// marshals to its MCP wire shape, omitting any unset optional field:
//
//	{"name":"demo","version":"1.0.0","title":"...","icons":[...]}
type Implementation struct {
	// Name is the implementation name. Required.
	Name string
	// Version is the implementation version. Required.
	Version string
	// Title is an optional human-friendly title.
	Title string
	// Description is an optional description.
	Description string
	// Icons is an optional list of icons.
	Icons []Icon
	// WebsiteURL is an optional website URL.
	WebsiteURL string
}

// NewImplementation constructs an Implementation from the required name and
// version. Optional fields are set via the returned value's exported fields or
// the With* helpers.
func NewImplementation(name, version string) Implementation {
	return Implementation{Name: name, Version: version}
}

// WithTitle returns a copy with the given title.
func (im Implementation) WithTitle(title string) Implementation {
	im.Title = title
	return im
}

// WithDescription returns a copy with the given description.
func (im Implementation) WithDescription(description string) Implementation {
	im.Description = description
	return im
}

// WithIcons returns a copy with the given icons.
func (im Implementation) WithIcons(icons ...Icon) Implementation {
	im.Icons = append([]Icon(nil), icons...)
	return im
}

// WithWebsiteURL returns a copy with the given website URL.
func (im Implementation) WithWebsiteURL(url string) Implementation {
	im.WebsiteURL = url
	return im
}

// ToMap renders the implementation as a map, omitting unset optional fields,
// mirroring laravel's Arr::whereNotNull behavior.
func (im Implementation) ToMap() map[string]any {
	m := map[string]any{
		"name":    im.Name,
		"version": im.Version,
	}
	if im.Title != "" {
		m["title"] = im.Title
	}
	if im.Description != "" {
		m["description"] = im.Description
	}
	if len(im.Icons) > 0 {
		icons := make([]map[string]any, len(im.Icons))
		for idx, ic := range im.Icons {
			icons[idx] = ic.ToMap()
		}
		m["icons"] = icons
	}
	if im.WebsiteURL != "" {
		m["websiteUrl"] = im.WebsiteURL
	}
	return m
}

// MarshalJSON implements json.Marshaler.
func (im Implementation) MarshalJSON() ([]byte, error) {
	return json.Marshal(im.ToMap())
}
