package transport

import (
	"net/http"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// statusForResponse maps a JSON-RPC reply onto the HTTP status the streamable
// HTTP transport prescribes for it: a method the server does not implement is
// 404, a server-side failure is 500, and any other error is 400. A successful
// result, and a reply that carries no error at all, is 200.
func statusForResponse(resp *jsonrpc.Response) int {
	if resp == nil || resp.Error == nil {
		return http.StatusOK
	}
	switch resp.Error.Code {
	case jsonrpc.CodeMethodNotFound:
		return http.StatusNotFound
	case jsonrpc.CodeInternalError:
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

// replyStatus is the HTTP status a reply to raw is written with. The mapping
// above applies only to a discovery-handshake request: such a client reads the
// outcome from the status as well as from the JSON-RPC error object. A legacy
// client predates that rule and may treat any non-2xx reply as a transport
// failure without reading its body, so its reply keeps the 200 that carries the
// error object to it intact.
func replyStatus(c *router.Context, raw []byte, resp *jsonrpc.Response) int {
	if !isModernRequest(c, raw) {
		return http.StatusOK
	}
	return statusForResponse(resp)
}

// isModernRequest reports whether a raw inbound message is a request made under
// the discovery handshake, which is to say one carrying protocol metadata in
// its params._meta.
func isModernRequest(c *router.Context, raw []byte) bool {
	info, ok := inspectedMessage(c, raw)
	return ok && !info.Legacy
}
