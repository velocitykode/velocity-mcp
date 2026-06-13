package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity/router"
)

// DefaultMaxBodyBytes is the default cap applied to an inbound HTTP request body
// on the MCP HTTP transport. A single JSON-RPC message is small in practice; a
// generous-but-bounded ceiling stops a hostile or buggy client from driving
// unbounded memory growth. Override per-handler with WithMaxBodyBytes.
const DefaultMaxBodyBytes int64 = 4 << 20 // 4 MiB

// sessionHeader is the HTTP header carrying the MCP session id (the
// "MCP-Session-Id" header). Header names are case-insensitive per RFC 7230, so
// the canonical Go form is used on read and write; clients sending
// "Mcp-Session-Id" are matched identically.
const sessionHeader = "Mcp-Session-Id"

// contentTypeJSON is the media type for a plain JSON-RPC reply body.
const contentTypeJSON = "application/json"

// httpOptions holds the tunable knobs for the HTTP transport handler.
type httpOptions struct {
	maxBodyBytes int64
}

// HandlerOption configures the HTTP transport handler returned by Handler.
type HandlerOption func(*httpOptions)

// WithMaxBodyBytes overrides the inbound request-body size cap (default
// DefaultMaxBodyBytes). A value <= 0 is ignored and the default is kept, so a
// caller cannot accidentally remove the limit (the body is always bounded).
func WithMaxBodyBytes(n int64) HandlerOption {
	return func(o *httpOptions) {
		if n > 0 {
			o.maxBodyBytes = n
		}
	}
}

// Handler returns a velocity router handler that serves the MCP server srv over
// streamable HTTP. Mount it on a POST route:
//
//	r.Post("/mcp", transport.Handler(srv))
//
// One POST carries one JSON-RPC message in its body. The handler reads the body
// (always wrapped in http.MaxBytesReader, default DefaultMaxBodyBytes), drives
// it through srv.Handle, and writes the reply:
//
//   - A request (has an id) yields a JSON-RPC response: HTTP 200 with the
//     response as the body (Content-Type application/json, or a single SSE
//     "data:" frame with Content-Type text/event-stream when the client asks
//     for an event stream via the Accept header).
//   - A notification (no id) yields no reply: HTTP 202 Accepted with an empty
//     body, per the MCP spec
//     (modelcontextprotocol.io/specification/.../transports#sending-messages-to-the-server).
//
// Session semantics: the inbound "Mcp-Session-Id" header (if any) is supplied
// to srv.Handle as the existing session id; an initialize response
// assigns a new session id which is echoed back in the "Mcp-Session-Id"
// response header. The per-route handler holds no session map of its own (the
// server owns session state), so there is no shared map to guard here; concrete
// transports that retain a session (Stdio, Fake) already mutex-protect it.
//
// Errors are never leaked to the client: a body read failure (including an
// oversized body) becomes a generic JSON-RPC parse/invalid-request error with
// no internal detail, and the real cause is logged server-side via the wired
// velocity logger when available. Malformed JSON is handled inside srv.Handle,
// which returns a JSON-RPC parse-error response (HTTP 200, error object).
//
// Authentication and authorization are deliberately NOT this handler's job.
// Apps attach their own velocity middleware to the route (auth guards, rate
// limiting, CORS, etc.); the MCP transport assumes the request has already been
// authorized by the surrounding middleware stack.
func Handler(srv MCPServer, opts ...HandlerOption) func(*router.Context) error {
	o := httpOptions{maxBodyBytes: DefaultMaxBodyBytes}
	for _, opt := range opts {
		opt(&o)
	}

	return func(c *router.Context) error {
		raw, readErr := readBody(c, o.maxBodyBytes)
		if readErr != nil {
			// An oversized or unreadable body is a client error. Report a
			// generic JSON-RPC parse error (no id context is recoverable from a
			// body we could not read) and log the real cause server-side. We
			// return the response with HTTP 200 so the JSON-RPC error object,
			// not an opaque transport status, reaches the client.
			logf(c, readErr)
			return writeParseError(c, readErr)
		}

		res := srv.Handle(c.Request.Context(), raw, inboundSessionID(c))

		// A notification (or any message that produces no reply) is acknowledged
		// with 202 Accepted and an empty body (the MCP spec mandates 202 for a
		// message that yields no response).
		if !res.HasResponse || res.Response == nil {
			return c.Status(http.StatusAccepted)
		}

		msg, err := encodeResponse(res.Response)
		if err != nil {
			// A response that cannot be marshalled is a server-side defect.
			// Surface a generic JSON-RPC internal error with no detail; log the
			// real cause server-side. HTTP stays 200 so the JSON-RPC error
			// object (not an opaque transport status) reaches the client.
			logf(c, err)
			return writeInternalError(c, res.Response)
		}

		if res.SessionID != "" {
			c.SetHeader(sessionHeader, res.SessionID)
		}

		if wantsEventStream(c) {
			return writeSSE(c, msg)
		}
		return writeJSON(c, msg)
	}
}

// readBody reads the inbound request body, wrapped in http.MaxBytesReader so a
// body exceeding maxBytes is rejected rather than buffered without bound. The
// reader is swapped onto the request so http.MaxBytesReader can signal the
// server to close the connection on overflow.
func readBody(c *router.Context, maxBytes int64) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	c.Request.Body = http.MaxBytesReader(c.Response, c.Request.Body, maxBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// inboundSessionID reads the MCP session id from the request header. Header
// lookup is case-insensitive, so a client sending any casing matches.
func inboundSessionID(c *router.Context) string {
	return strings.TrimSpace(c.Request.Header.Get(sessionHeader))
}

// wantsEventStream reports whether the client asked for a Server-Sent Events
// reply via the Accept header. The MCP streamable-HTTP spec lets a client
// negotiate an event stream; absent text/event-stream we reply with plain JSON.
//
// A client that accepts application/json is served JSON even when it also lists
// text/event-stream, so the lighter single-message representation wins
// (application/json is sorted ahead of the event stream before the header is
// inspected). Only when the client asks for text/event-stream and does NOT also
// accept application/json (nor the "*/*" wildcard) do we switch to the SSE
// framing.
func wantsEventStream(c *router.Context) bool {
	accept := c.Request.Header.Get("Accept")
	if accept == "" {
		return false
	}
	var sse, json bool
	for _, part := range strings.Split(accept, ",") {
		media := part
		if i := strings.IndexByte(media, ';'); i >= 0 {
			media = media[:i]
		}
		media = strings.ToLower(strings.TrimSpace(media))
		switch media {
		case "text/event-stream":
			sse = true
		case contentTypeJSON, "application/*", "*/*":
			json = true
		}
	}
	return sse && !json
}

// writeJSON writes a JSON-RPC reply as a plain application/json body with HTTP
// 200. The body is the already-encoded message frame; a trailing newline is
// added for parity with line-oriented clients and curl readability.
func writeJSON(c *router.Context, msg []byte) error {
	c.SetHeader("Content-Type", contentTypeJSON)
	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.Response.WriteHeader(http.StatusOK)
	if _, err := c.Response.Write(msg); err != nil {
		return err
	}
	_, err := c.Response.Write([]byte{'\n'})
	return err
}

// writeSSE writes a JSON-RPC reply as a single Server-Sent Events "data:" frame
// with Content-Type text/event-stream and HTTP 200, as a single "data:
// <message>\n\n" frame. This is the streamable-HTTP SSE response mode; the
// framework's PrepareStreamHeaders sets
// the standard streaming headers (and X-Accel-Buffering: no) so proxies do not
// buffer the stream.
func writeSSE(c *router.Context, msg []byte) error {
	router.PrepareStreamHeaders(c.Response)
	c.Response.WriteHeader(http.StatusOK)
	if _, err := c.Response.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := c.Response.Write(msg); err != nil {
		return err
	}
	if _, err := c.Response.Write([]byte("\n\n")); err != nil {
		return err
	}
	if f, ok := c.Response.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// writeParseError writes a generic JSON-RPC parse-error response with HTTP 200.
// No id can be recovered from a body we could not read, so a null id is used,
// matching how srv.Handle reports an unparseable message. No internal detail
// from the underlying read error reaches the client.
func writeParseError(c *router.Context, cause error) error {
	const body = `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error: Invalid JSON was received by the server."}}`
	c.SetHeader("Content-Type", contentTypeJSON)
	c.SetHeader("X-Content-Type-Options", "nosniff")
	c.Response.WriteHeader(http.StatusOK)
	_, err := c.Response.Write([]byte(body + "\n"))
	return err
}

// writeInternalError writes a generic JSON-RPC internal-error response with
// HTTP 200, preserving the id of the original response so a compliant client can
// correlate it. No internal detail reaches the client. Used only on the
// vanishingly rare path where an already-built response cannot be re-marshalled.
func writeInternalError(c *router.Context, orig *jsonrpc.Response) error {
	id := jsonrpc.NullID()
	if orig != nil {
		id = orig.ID
	}
	resp := jsonrpc.NewErrorResponseCode(id, jsonrpc.CodeInternalError, "Something went wrong while processing the request.")
	msg, err := encodeResponse(resp)
	if err != nil {
		// Even the canned error failed to marshal: fall back to a static body.
		msg = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Something went wrong while processing the request."}}`)
	}
	return writeJSON(c, msg)
}

// logf reports a transport-level error to the wired velocity logger when one is
// available, degrading to a no-op when services are not wired (e.g. a raw
// NewTestContext). It never panics: ServicesIfSet returns nil rather than
// crashing when the container is absent.
func logf(c *router.Context, err error) {
	if err == nil {
		return
	}
	s := c.ServicesIfSet()
	if s == nil || s.Log == nil {
		return
	}
	s.Log.Error("mcp http transport error", "error", err.Error())
}

// maxBytesError reports whether err is the sentinel returned by
// http.MaxBytesReader when the body exceeds the configured limit. Exposed as a
// helper so tests can assert the oversized-body path without string matching.
func maxBytesError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
