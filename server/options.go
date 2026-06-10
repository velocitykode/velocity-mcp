package server

import (
	"strings"

	"github.com/velocitykode/velocity/contract"

	"github.com/velocitykode/velocity-mcp/schema"
)

// WithInstructions sets the server instructions string advertised during the
// initialize handshake.
func WithInstructions(instructions string) Option {
	return func(s *Server) { s.instructions = instructions }
}

// WithTitle sets a human-friendly title for the server implementation metadata.
func WithTitle(title string) Option {
	return func(s *Server) { s.title = title }
}

// WithWebsiteURL sets the server implementation website URL.
func WithWebsiteURL(url string) Option {
	return func(s *Server) { s.websiteURL = url }
}

// WithIcons sets the server implementation icons.
func WithIcons(icons ...schema.Icon) Option {
	return func(s *Server) { s.icons = append([]schema.Icon(nil), icons...) }
}

// WithTools registers the given tools. It may be called via multiple options;
// each appends to the set.
func WithTools(tools ...Tool) Option {
	return func(s *Server) {
		for _, t := range tools {
			if t != nil {
				s.tools = append(s.tools, t)
			}
		}
	}
}

// WithResources registers the given resources, routing URI-template resources
// to the templates set (reported under resources/templates/list) and the rest
// to the plain resources set, mirroring laravel/mcp's ServerContext split.
func WithResources(resources ...Resource) Option {
	return func(s *Server) {
		for _, r := range resources {
			if r == nil {
				continue
			}
			if tmpl, ok := r.(URITemplate); ok {
				s.templates = append(s.templates, tmpl)
				continue
			}
			s.resources = append(s.resources, r)
		}
	}
}

// WithPrompts registers the given prompts.
func WithPrompts(prompts ...Prompt) Option {
	return func(s *Server) {
		for _, p := range prompts {
			if p != nil {
				s.prompts = append(s.prompts, p)
			}
		}
	}
}

// WithPageSize sets the default pagination page size for list methods. A value
// of zero or less is ignored. The effective page size is still capped by the
// maximum (see WithMaxPageSize).
func WithPageSize(size int) Option {
	return func(s *Server) {
		if size > 0 {
			s.defaultPageSize = size
		}
	}
}

// WithMaxPageSize sets the maximum pagination page size. A value of zero or less
// is ignored.
func WithMaxPageSize(size int) Option {
	return func(s *Server) {
		if size > 0 {
			s.maxPageSize = size
		}
	}
}

// WithCapability adds or overrides an advertised capability. A key containing a
// dot ("feature.enabled") sets a nested boolean capability; a plain key
// registers an empty-object capability, mirroring laravel/mcp's
// Server::addCapability. Use this to opt into capabilities not enabled by
// default, such as completions.
func WithCapability(key string, value ...bool) Option {
	v := true
	if len(value) > 0 {
		v = value[0]
	}
	return func(s *Server) {
		if s.capabilities == nil {
			s.capabilities = map[string]any{}
		}
		if root, child, ok := strings.Cut(key, "."); ok {
			existing, _ := s.capabilities[root].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			existing[child] = v
			s.capabilities[root] = existing
			return
		}
		// A bare key registers an empty-object capability, mirroring laravel's
		// representation of an enabled-but-empty capability.
		s.capabilities[key] = map[string]any{}
	}
}

// WithProtocolVersions overrides the supported protocol version list. The order
// is significant: the first element is the negotiation fallback. An empty list
// is ignored (the defaults are retained).
func WithProtocolVersions(versions ...ProtocolVersion) Option {
	return func(s *Server) {
		if len(versions) > 0 {
			s.versions = append([]ProtocolVersion(nil), versions...)
		}
	}
}

// WithLogger sets the logger used for server-side diagnostics (e.g. event
// dispatch failures). Errors are never leaked to clients; they are logged here.
func WithLogger(logger contract.Logger) Option {
	return func(s *Server) { s.logger = logger }
}
