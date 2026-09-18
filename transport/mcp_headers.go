package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/velocitykode/velocity/router"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// MCP request headers that mirror the JSON-RPC body. A proxy, gateway, or cache
// in front of the server can route and bill a request from its headers alone,
// which only works if the headers cannot disagree with the body they describe.
const (
	// HeaderProtocolVersion mirrors params._meta protocol version.
	HeaderProtocolVersion = "MCP-Protocol-Version"
	// HeaderMethod mirrors the JSON-RPC method.
	HeaderMethod = "Mcp-Method"
	// HeaderName mirrors the request's named target: params.name for
	// tools/call and prompts/get, params.uri for resources/read. Its value may
	// be base64-wrapped (see EncodeHeaderValue) when the name is not
	// header-safe.
	HeaderName = "Mcp-Name"
)

// ValidateHeaders returns velocity router middleware enforcing that the MCP
// request headers mirror the JSON-RPC body.
//
// A discovery-handshake request must carry MCP-Protocol-Version and Mcp-Method,
// and, when it addresses a named target (tools/call, prompts/get,
// resources/read), Mcp-Name as well. A header that is absent or contradicts the
// body fails the request with HTTP 400 and a JSON-RPC error of code -32020,
// correlated to the request id so a client can match it to the call it made.
//
// Which rules a request falls under is settled from the header and the body
// together. A legacy request, one carrying no protocol metadata in params._meta
// and either no MCP-Protocol-Version header or one naming a revision the
// initialize handshake still negotiates, is exempt: those clients predate the
// headers. A request whose header declares a discovery-era revision while its
// body declares none is refused rather than exempted, because the two halves
// describe different protocols and an intermediary authorizing the call from
// its headers would be reading a declaration the server itself does not honour.
// A header naming any other revision is answered with HTTP 400 and -32022, as
// the streamable HTTP transport requires of a protocol version the server does
// not speak. Anything that is not a JSON-RPC request with a usable id and method
// is passed through untouched, so the server produces the proper parse or
// invalid-request error rather than a header complaint.
//
// The body is read here and handed on intact, bounded by the same cap the
// handler applies (DefaultMaxBodyBytes unless WithMaxBodyBytes overrides it),
// so reading it for validation cannot become a way to smuggle an unbounded body
// past the limit.
func ValidateHeaders(opts ...HandlerOption) router.MiddlewareFunc {
	o := httpOptions{maxBodyBytes: DefaultMaxBodyBytes}
	for _, opt := range opts {
		opt(&o)
	}

	return func(next router.HandlerFunc) router.HandlerFunc {
		return func(c *router.Context) error {
			raw, err := readBody(c, o.maxBodyBytes)
			if err != nil {
				logf(c, err)
				return writeParseError(c, err)
			}
			// Hand the buffered body on: the handler reads it again.
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))

			info, ok := server.InspectMessage(raw)
			if ok {
				// The handler routes on the same inspection rather than decoding
				// the body a second time, so the two can never classify one
				// message differently and a hostile body is parsed once here.
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), inspectedMessageKey{}, &info))
			}
			if !ok {
				return next(c)
			}
			if info.Legacy {
				return legacyRequest(c, info, next)
			}
			if message := headerMismatch(c, info); message != "" {
				return writeHeaderMismatch(c, info.ID, message)
			}
			return next(c)
		}
	}
}

// inspectedMessageKey keys the MessageInfo the header middleware already
// derived from the request body. It is an unexported zero-size type, so nothing
// outside this package can read or overwrite the value.
type inspectedMessageKey struct{}

// inspectedMessage returns the MessageInfo the header middleware stored for
// this request, or inspects raw when the handler is mounted without it.
func inspectedMessage(c *router.Context, raw []byte) (server.MessageInfo, bool) {
	if info, ok := c.Request.Context().Value(inspectedMessageKey{}).(*server.MessageInfo); ok && info != nil {
		return *info, true
	}
	return server.InspectMessage(raw)
}

// legacyRequest applies the header rules to a request whose body declares no
// protocol metadata, deciding from the MCP-Protocol-Version header alone which
// protocol it claims to speak.
//
// A request stating no version is served: the header postdates the revisions
// that omit it. A version the initialize handshake still negotiates is served
// too, without the mirrored Mcp-Method and Mcp-Name checks, because a client of
// that revision has no metadata in its body for them to mirror. A header naming
// the discovery revision contradicts a body declaring none, which is the
// disagreement these headers exist to rule out. Anything else names a protocol
// this server does not speak, and is refused with -32022 rather than run under
// the exemption: the exemption speaks for the revisions listed above and for no
// others, and a request admitted through it is one no header check has seen.
func legacyRequest(c *router.Context, info server.MessageInfo, next router.HandlerFunc) error {
	declared, message := statedProtocolVersion(c)
	if message != "" {
		return writeHeaderMismatch(c, info.ID, message)
	}
	switch {
	case declared == "":
		return next(c)
	case server.HandshakeFor(declared) == server.HandshakeDiscovery:
		return writeHeaderMismatch(c, info.ID, undeclaredProtocolMessage(declared))
	case supportsVersion(server.InitializeSupportedVersions(), declared):
		return next(c)
	default:
		return writeUnsupportedProtocolVersion(c, info.ID, declared)
	}
}

// statedProtocolVersion returns the protocol revision the MCP-Protocol-Version
// header states, or "" when the request states none.
//
// A value that is not a well-formed field value yields the complaint about it
// instead. Such a header states nothing in any revision, so which era the body
// belongs to does not enter into it: the request is refused rather than read as
// one declaring no version at all, which is how an intermediary reading a
// normalized form of that value would end up authorizing a call the server
// answered under different rules.
func statedProtocolVersion(c *router.Context) (string, string) {
	value, message := statedHeader(c, HeaderProtocolVersion)
	if message != "" {
		return "", message
	}
	if value == "" {
		return "", ""
	}
	return headerValue(HeaderProtocolVersion, value, false)
}

// supportsVersion reports whether list names version.
func supportsVersion(list []server.ProtocolVersion, version string) bool {
	for _, item := range list {
		if item == version {
			return true
		}
	}
	return false
}

// headerProtocolVersions returns the revisions the MCP-Protocol-Version header
// may name: the ones whose requests mirror their metadata into these headers,
// and the ones the initialize handshake still negotiates. It is what a -32022
// reports as supported, so a client reading it learns every version it may
// state here.
func headerProtocolVersions() []server.ProtocolVersion {
	return append(server.ServerSupportedVersions(), server.InitializeSupportedVersions()...)
}

// undeclaredProtocolMessage builds the complaint about a request whose header
// declares a discovery-era revision that its body does not. The version named is
// one of the revisions this server knows, so nothing of the request's own making
// is echoed back.
func undeclaredProtocolMessage(declared string) string {
	return "Header mismatch: The [" + HeaderProtocolVersion + "] header declares protocol version [" +
		declared + "] but the request body declares no protocol version."
}

// headerMismatch returns the complaint about the first header that is absent or
// contradicts the body, or "" when every header mirrors it. The headers are
// checked in the order a client sets them, so the message names the first
// problem rather than an arbitrary one.
func headerMismatch(c *router.Context, info server.MessageInfo) string {
	if m := checkHeader(c, HeaderProtocolVersion, info.ProtocolVersion, info.HasProtocolVersion, true, false); m != "" {
		return m
	}
	if m := checkHeader(c, HeaderMethod, info.Method, true, true, false); m != "" {
		return m
	}
	return checkHeader(c, HeaderName, info.Name, info.HasName, info.RequiresName, true)
}

// checkHeader compares one header against the body value it mirrors. An absent
// header is a failure only when the method requires it. A header carrying a
// value the body does not state at all (stated is false, because the member is
// absent or ill-typed) is accepted: the body is then malformed in its own right
// and the method handler reports that in the client's own terms rather than the
// header machinery guessing at it. A member the body states as an empty string
// is stated, so a header carrying anything else contradicts it. decode selects
// the headers whose values may arrive base64-wrapped.
//
// A header whose value is not well formed fails before any comparison, because
// a value the encoding cannot express states nothing to compare, whatever the
// body happens to spell. Only the optional whitespace RFC 9110 permits around a
// field value is removed first (see trimOWS); every other byte reaches the
// check, so a value cannot slip past it by padding itself with bytes no encoder
// would have written.
func checkHeader(c *router.Context, name, expected string, stated, required, decode bool) string {
	value, message := statedHeader(c, name)
	if message != "" {
		return message
	}
	if value == "" {
		if required {
			return "Header mismatch: The [" + name + "] header is required."
		}
		return ""
	}
	value, message = headerValue(name, value, decode)
	if message != "" {
		return message
	}
	if !stated || value == expected {
		return ""
	}
	return "Header mismatch: The [" + name + "] header value [" + value + "] does not match the request body value [" + expected + "]."
}

// statedHeader reads the single field value a request states for an MCP header,
// or the complaint about a request stating it more than once.
//
// Each of these headers mirrors one member of the body, so it has one value. A
// request carrying two field lines for it states neither: RFC 9110 section 5.3
// lets a recipient join repeated lines into one comma-separated value, so the
// intermediary routing on the header and the server reading the body would be
// looking at different things, which is the disagreement these headers exist to
// rule out. Reading the first line and discarding the rest is what makes that
// disagreement invisible, so the request is refused instead.
//
// The optional whitespace RFC 9110 section 5.6.3 permits around a field value is
// removed here (see trimOWS); every other byte reaches the caller.
func statedHeader(c *router.Context, name string) (string, string) {
	lines := c.Request.Header.Values(name)
	if len(lines) > 1 {
		return "", "Header mismatch: The [" + name + "] header is stated more than once."
	}
	if len(lines) == 0 {
		return "", ""
	}
	return trimOWS(lines[0]), ""
}

// headerValue reads the value a header states, or the complaint about a value
// no peer following the encoding could have written. Every MCP header value is a
// field value; the headers whose values may arrive base64-wrapped (decode) must
// in addition carry a payload that decodes. Accepting either malformed form on
// the strength of the body spelling it the same way would let a name travel in a
// form the encoding does not define, which an intermediary routing on the header
// would resolve differently from the server reading the body.
//
// Neither complaint echoes the offending value: it is by definition not text
// this server would write back into a header, and the client already holds it.
func headerValue(name, value string, decode bool) (string, string) {
	if !decode {
		if !isLiteralHeaderValue(value) {
			return "", malformedHeaderMessage(name, "is not a valid header value")
		}
		return value, ""
	}
	decoded, err := DecodeHeaderValue(value)
	if errors.Is(err, ErrHeaderValueEncoding) {
		return "", malformedHeaderMessage(name, "declares a base64 encoding that does not decode")
	}
	if err != nil {
		return "", malformedHeaderMessage(name, "is not a valid header value")
	}
	return decoded, ""
}

// malformedHeaderMessage builds the complaint about a header value the server
// cannot read, naming the header and the fault but not the value itself.
func malformedHeaderMessage(name, fault string) string {
	return "Header mismatch: The [" + name + "] header value " + fault + "."
}

// msgUnsupportedProtocolVersion is the message of the -32022 a header names a
// protocol version this server does not speak with. The stated and the accepted
// versions travel in the error data rather than the message, matching the -32022
// the server itself raises over a request's protocol metadata.
const msgUnsupportedProtocolVersion = "Unsupported protocol version"

// writeHeaderMismatch answers a header failure with HTTP 400 and a JSON-RPC
// error carrying the mismatch code. The message names the header and the two
// values in conflict, both of which came from the request itself, so nothing
// internal is disclosed.
func writeHeaderMismatch(c *router.Context, rawID []byte, message string) error {
	return writeHeaderError(c, rawID, jsonrpc.NewError(jsonrpc.CodeHeaderMismatch, message))
}

// writeUnsupportedProtocolVersion answers an MCP-Protocol-Version header naming
// a revision this server does not speak with HTTP 400 and -32022, carrying the
// versions the header may name and the one it stated so the client can restate
// it. The stated value has already been checked to be a well-formed field value,
// and it is the client's own, so echoing it discloses nothing.
func writeUnsupportedProtocolVersion(c *router.Context, rawID []byte, stated string) error {
	return writeHeaderError(c, rawID, jsonrpc.NewError(jsonrpc.CodeUnsupportedProtocolVersion, msgUnsupportedProtocolVersion).
		WithData(map[string]any{
			"supported": headerProtocolVersions(),
			"requested": stated,
		}))
}

// writeHeaderError writes a header failure as HTTP 400 and a JSON-RPC error
// correlated to the request id exactly as it arrived. The body echoes
// request-controlled text, so it is served with the same content-type pinning as
// every other reply this transport writes.
func writeHeaderError(c *router.Context, rawID []byte, rpcErr *jsonrpc.Error) error {
	id := jsonrpc.NullID()
	// The id token is preserved as it arrived, so a numeric id stays numeric and
	// a string id stays a string. Only a token the specification permits in a
	// response is echoed; anything else leaves the null id in place, exactly as
	// the server does when it declines to correlate a reply.
	var echoed jsonrpc.ID
	if err := echoed.UnmarshalJSON(rawID); err == nil && echoed.IsValidRequestID() {
		id = echoed
	}
	c.SetHeader("X-Content-Type-Options", "nosniff")
	return c.JSON(http.StatusBadRequest, jsonrpc.NewErrorResponse(id, rpcErr))
}

// wantsSubscriptionStream reports whether a raw inbound message opens a
// subscription. Such a request is answered with a notification followed by its
// result, so it needs the streamed response path to deliver both frames.
func wantsSubscriptionStream(c *router.Context, raw []byte) bool {
	info, ok := inspectedMessage(c, raw)
	return ok && info.Method == "subscriptions/listen"
}
