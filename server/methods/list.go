package methods

import (
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// ListTools handles "tools/list": a cursor-paginated list of registered tools.
type ListTools struct{}

var _ server.Method = ListTools{}

// Handle paginates the tool set and returns it under the "tools" key.
func (ListTools) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)
	items := make([]map[string]any, 0, len(c.Tools()))
	for _, t := range c.Tools() {
		items = append(items, toolToMap(t))
	}
	return paginatedResult(req, "tools", items, c.PerPage(p.intPtr("per_page")), p.str("cursor"))
}

// ListResources handles "resources/list": a cursor-paginated list of registered
// non-template resources.
type ListResources struct{}

var _ server.Method = ListResources{}

// Handle paginates the resource set and returns it under the "resources" key.
func (ListResources) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)
	items := make([]map[string]any, 0, len(c.Resources()))
	for _, r := range c.Resources() {
		items = append(items, resourceToMap(r))
	}
	return paginatedResult(req, "resources", items, c.PerPage(p.intPtr("per_page")), p.str("cursor"))
}

// ListResourceTemplates handles "resources/templates/list": a cursor-paginated
// list of registered URI-template resources.
type ListResourceTemplates struct{}

var _ server.Method = ListResourceTemplates{}

// Handle paginates the template set and returns it under the "resourceTemplates"
// key.
func (ListResourceTemplates) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)
	items := make([]map[string]any, 0, len(c.ResourceTemplates()))
	for _, r := range c.ResourceTemplates() {
		items = append(items, templateToMap(r))
	}
	return paginatedResult(req, "resourceTemplates", items, c.PerPage(p.intPtr("per_page")), p.str("cursor"))
}

// ListPrompts handles "prompts/list": a cursor-paginated list of registered
// prompts.
type ListPrompts struct{}

var _ server.Method = ListPrompts{}

// Handle paginates the prompt set and returns it under the "prompts" key.
func (ListPrompts) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	p := decode(req)
	items := make([]map[string]any, 0, len(c.Prompts()))
	for _, pr := range c.Prompts() {
		items = append(items, promptToMap(pr))
	}
	return paginatedResult(req, "prompts", items, c.PerPage(p.intPtr("per_page")), p.str("cursor"))
}

// paginatedResult slices items into a page and builds the JSON-RPC result,
// emitting the page under key and a "nextCursor" only when more pages remain.
func paginatedResult(req *jsonrpc.Request, key string, items []map[string]any, perPage int, cursor string) (*jsonrpc.Response, error) {
	page, next := server.NewCursorPaginator(items, perPage, cursor).Paginate()
	result := map[string]any{key: page}
	if next != "" {
		result["nextCursor"] = next
	}
	return jsonrpc.NewResult(req.ID, result)
}
