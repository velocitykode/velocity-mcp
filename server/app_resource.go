package server

import (
	"context"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/ui"
)

// AppResourceMimeType is the MIME type that marks a resource as an MCP UI app:
// HTML the host renders in a sandboxed application frame rather than displaying
// as plain text.
const AppResourceMimeType = "text/html;profile=mcp-app"

// AppResource is implemented by resources that are MCP UI apps. The method
// handlers surface the metadata under the "_meta.ui" key of the resource's
// resources/list entry so a host can size the frame, grant permissions, and
// apply the Content Security Policy. A resource that does not implement it is
// treated as an ordinary resource.
type AppResource interface {
	AppMeta() ui.AppMeta
}

// AppResourceBuilder is a closure-based AppResource: an MCP UI app resource
// defined inline by a URI, optional metadata, and an HTML-producing handler,
// rather than a struct implementing the interfaces. It mirrors ToolBuilder's
// ergonomics.
//
// Build one with NewAppResource and configure it with WithDescription,
// WithAppMeta, and HTMLFunc. The builder reports the app MIME type, advertises
// its app metadata, and on read injects the configured libraries' script tags
// into the returned HTML so the document loads them without the author writing
// the tags.
type AppResourceBuilder struct {
	name        string
	description string
	uri         string
	appMeta     ui.AppMeta
	htmlFn      func(ctx context.Context, req *Request) (string, error)
}

// Compile-time assertions that *AppResourceBuilder satisfies Resource and
// AppResource.
var (
	_ Resource    = (*AppResourceBuilder)(nil)
	_ AppResource = (*AppResourceBuilder)(nil)
)

// NewAppResource starts building an app resource with the given kebab-case name
// and URI. App resource URIs use the "ui://" scheme by convention (e.g.
// "ui://dashboard").
func NewAppResource(name, uri string) *AppResourceBuilder {
	return &AppResourceBuilder{name: name, uri: uri, appMeta: ui.NewAppMeta()}
}

// WithDescription sets the human-readable description and returns the builder.
func (a *AppResourceBuilder) WithDescription(description string) *AppResourceBuilder {
	a.description = description
	return a
}

// WithAppMeta sets the app metadata (CSP, permissions, domain, libraries) and
// returns the builder.
func (a *AppResourceBuilder) WithAppMeta(meta ui.AppMeta) *AppResourceBuilder {
	a.appMeta = meta
	return a
}

// HTMLFunc sets the handler that produces the app's HTML body for a read. The
// builder injects the configured libraries' script tags ahead of the returned
// body, so the handler returns only its own markup.
func (a *AppResourceBuilder) HTMLFunc(fn func(ctx context.Context, req *Request) (string, error)) *AppResourceBuilder {
	a.htmlFn = fn
	return a
}

// Name returns the resource name.
func (a *AppResourceBuilder) Name() string { return a.name }

// Description returns the resource description.
func (a *AppResourceBuilder) Description() string { return a.description }

// URI returns the resource URI.
func (a *AppResourceBuilder) URI() string { return a.uri }

// MimeType returns the app resource MIME type.
func (a *AppResourceBuilder) MimeType() string { return AppResourceMimeType }

// AppMeta implements AppResource, returning the configured app metadata.
func (a *AppResourceBuilder) AppMeta() ui.AppMeta { return a.appMeta }

// Read produces the app's HTML, prefixed with the configured libraries' script
// tags. A builder with no HTMLFunc returns a tool-style error result rather than
// panicking, so a half-built app resource degrades gracefully.
func (a *AppResourceBuilder) Read(ctx context.Context, req *Request) (*Response, error) {
	if a.htmlFn == nil {
		return Error("App resource has no HTML handler."), nil
	}
	body, err := a.htmlFn(ctx, req)
	if err != nil {
		return nil, err
	}
	if scripts := a.appMeta.ScriptTags(); scripts != "" {
		body = scripts + "\n" + body
	}
	return NewResponse(content.NewText(body)), nil
}
