// Package methods implements the MCP protocol method handlers: initialize,
// ping, tools/list, tools/call, resources/list, resources/read,
// resources/templates/list, prompts/list, prompts/get, completion/complete.
//
// Each handler implements server.Method. Importing this package installs the
// full method set on every server.Server via server.SetMethodFactory (called in
// init), so a program that imports methods (as the transport does) gets the
// complete protocol wired automatically without the server package importing
// methods (which would create an import cycle).
//
// Dispatch ownership: the server package owns message routing (server.Handle
// maps method name to handler and special-cases initialize and tools/call so it
// can emit MCP events). This package owns the per-method request/response
// translation.
package methods

import (
	"github.com/velocitykode/velocity-mcp/server"
)

// init installs the default method set on every server (plus initialize and
// ping, which the server also serves directly).
func init() {
	server.SetMethodFactory(defaultMethods)
}

// defaultMethods builds a fresh map of method name to handler. The handlers are
// stateless, so a single shared instance per name is reused across servers.
func defaultMethods() map[string]server.Method {
	return map[string]server.Method{
		"initialize":               InitializeMethod{},
		"ping":                     Ping{},
		"tools/list":               ListTools{},
		"tools/call":               CallTool{},
		"resources/list":           ListResources{},
		"resources/read":           ReadResource{},
		"resources/templates/list": ListResourceTemplates{},
		"prompts/list":             ListPrompts{},
		"prompts/get":              GetPrompt{},
		"completion/complete":      CompletionComplete{},
	}
}
