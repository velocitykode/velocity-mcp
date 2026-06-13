package content

import "encoding/json"

// ResourceLink references a resource by uri and name without embedding its
// contents. It may be used in tool and prompt results but not in a resource
// (resources/read) context, where ToResource returns ErrNotAllowed.
//
// Wire shape: {"type":"resource_link","uri":...,"name":...,...}. Optional
// fields (title, description, mimeType, size, annotations, icons) are omitted
// when unset.
type ResourceLink struct {
	meta
	uri         string
	name        string
	mimeType    string
	title       string
	description string
	size        *int
	annotations map[string]any
	icons       []Icon
}

// NewResourceLink constructs a ResourceLink with the required uri and name.
// Optional fields are set via the With* builder methods.
func NewResourceLink(uri, name string) *ResourceLink {
	return &ResourceLink{uri: uri, name: name}
}

// WithMimeType sets the optional mime type and returns the link for chaining.
func (r *ResourceLink) WithMimeType(mimeType string) *ResourceLink {
	r.mimeType = mimeType
	return r
}

// WithTitle sets the optional title and returns the link for chaining.
func (r *ResourceLink) WithTitle(title string) *ResourceLink {
	r.title = title
	return r
}

// WithDescription sets the optional description and returns the link for
// chaining.
func (r *ResourceLink) WithDescription(description string) *ResourceLink {
	r.description = description
	return r
}

// WithSize sets the optional size (in bytes) and returns the link for chaining.
func (r *ResourceLink) WithSize(size int) *ResourceLink {
	r.size = &size
	return r
}

// WithAnnotations sets the optional annotations map and returns the link for
// chaining.
func (r *ResourceLink) WithAnnotations(annotations map[string]any) *ResourceLink {
	r.annotations = annotations
	return r
}

// WithIcons sets the optional list of icons and returns the link for chaining.
func (r *ResourceLink) WithIcons(icons ...Icon) *ResourceLink {
	r.icons = icons
	return r
}

// String returns the resource uri.
func (r *ResourceLink) String() string { return r.uri }

// toArray returns the resource_link wire shape, omitting unset optional fields.
func (r *ResourceLink) toArray() map[string]any {
	data := map[string]any{
		"type": "resource_link",
		"uri":  r.uri,
		"name": r.name,
	}
	if r.title != "" {
		data["title"] = r.title
	}
	if r.description != "" {
		data["description"] = r.description
	}
	if r.mimeType != "" {
		data["mimeType"] = r.mimeType
	}
	if r.size != nil {
		data["size"] = *r.size
	}
	if len(r.annotations) > 0 {
		ann := make(map[string]any, len(r.annotations))
		for k, v := range r.annotations {
			ann[k] = v
		}
		data["annotations"] = ann
	}
	if len(r.icons) > 0 {
		icons := make([]map[string]any, len(r.icons))
		for i, ic := range r.icons {
			icons[i] = ic.toArray()
		}
		data["icons"] = icons
	}
	return r.merge(data)
}

// MarshalJSON encodes the default MCP wire shape.
func (r *ResourceLink) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.toArray())
}

// ToTool returns the tools/call result shape.
func (r *ResourceLink) ToTool() (map[string]any, error) { return r.toArray(), nil }

// ToPrompt returns the prompts/get message shape.
func (r *ResourceLink) ToPrompt() (map[string]any, error) { return r.toArray(), nil }

// ToResource returns ErrNotAllowed: resource_link content may not be used in
// resources.
func (r *ResourceLink) ToResource(uri, mimeType string) (map[string]any, error) {
	return nil, ErrNotAllowed
}
