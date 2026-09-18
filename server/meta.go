package server

import (
	"bytes"
	"encoding/json"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
)

// Reserved _meta keys defined by the MCP specification. They are namespaced
// under "io.modelcontextprotocol/" so an application's own _meta keys can never
// collide with them.
const (
	// MetaKeyProtocolVersion carries the protocol revision a request is made
	// under. Its presence marks the request as a discovery-handshake request.
	MetaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	// MetaKeyClientCapabilities carries the capabilities the client declares
	// for the request. Its presence marks the request as a discovery-handshake
	// request.
	MetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	// MetaKeyClientInfo carries the client implementation metadata.
	MetaKeyClientInfo = "io.modelcontextprotocol/clientInfo"
	// MetaKeyServerInfo carries the server implementation metadata the server
	// attaches to every result.
	MetaKeyServerInfo = "io.modelcontextprotocol/serverInfo"
	// MetaKeySubscriptionID correlates subscription notifications with the
	// subscriptions/listen request that opened them.
	MetaKeySubscriptionID = "io.modelcontextprotocol/subscriptionId"
)

// ExtensionUI is the MCP Apps extension key, advertised under
// capabilities.extensions when the server registers an app resource.
const ExtensionUI = "io.modelcontextprotocol/ui"

// msgUnsupportedVersion is the message of the -32022 error. The offending and
// accepted versions travel in the error data rather than the message.
const msgUnsupportedVersion = "Unsupported protocol version"

// namedParamKey maps a method to the params member naming its target, or "" for
// a method that addresses no single primitive. It is the same mapping the
// Mcp-Name HTTP header mirrors.
func namedParamKey(method string) string {
	switch method {
	case "tools/call", "prompts/get":
		return "name"
	case "resources/read":
		return "uri"
	default:
		return ""
	}
}

// requiresProtocolMeta reports whether a method exists only in the discovery
// handshake and so must state its protocol metadata in every case. The legacy
// exemption speaks for clients that predate that metadata, and no such client
// can be calling a method that arrived with it: these two have no legacy form to
// be exempt as.
func requiresProtocolMeta(method string) bool {
	switch method {
	case "server/discover", "subscriptions/listen":
		return true
	default:
		return false
	}
}

// readsArgumentBag reports whether the specification gives a method an
// [arguments] member: the bag a tool call, a prompt render, or a resource read
// is invoked with. Every other method, including one a server registers itself,
// owns its parameter schema and may model an [arguments] member however it
// likes.
func readsArgumentBag(method string) bool {
	switch method {
	case "tools/call", "prompts/get", "resources/read":
		return true
	default:
		return false
	}
}

// rawMembers is a decoded JSON object whose members are kept in the form they
// arrived in.
//
// Protocol metadata is read through it rather than through a map[string]any
// because decoding into Go values fails on members no Go value can hold: a
// number outside float64's range (1e10000) is valid JSON that encoding/json
// refuses. One such member anywhere in params would fail the whole decode, and
// every reader below would then see a request that declares no protocol
// metadata at all, which is exactly the shape that is exempt from validation.
// Keeping the members raw means an unreadable member costs only itself.
type rawMembers = map[string]json.RawMessage

// objectMembers decodes a JSON object into its members, leaving each member's
// bytes untouched. It reports false for anything that is not a JSON object,
// including the empty array some encoders render an empty object as: that form
// carries no members either way, so nothing is lost by declining it here.
func objectMembers(raw json.RawMessage) (rawMembers, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var members rawMembers
	if err := json.Unmarshal(trimmed, &members); err != nil || members == nil {
		return nil, false
	}
	return members, true
}

// stringMember decodes a member the protocol models as a string, reporting
// false when the member is absent or carries any other JSON type. The token is
// inspected before decoding, because encoding/json reads a JSON null into a
// string without complaint and would report a member that states nothing as a
// stated empty string.
func stringMember(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

// isObjectMember reports whether a raw member is one the protocol accepts where
// it models an object (the client capabilities, a request's argument bag). An
// empty JSON array is accepted alongside an object because some encoders render
// an empty map that way and it decodes to the same empty set; this is the same
// tolerance the [params] member itself is parsed with. A non-empty array, a
// scalar, and a null are rejected.
func isObjectMember(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{':
		return json.Valid(trimmed)
	case '[':
		return json.Valid(trimmed) && len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) == 0
	default:
		return false
	}
}

// MessageInfo is the subset of an inbound JSON-RPC request a transport needs in
// order to validate the mirrored MCP HTTP headers without handling the message
// itself. It is produced by InspectMessage.
type MessageInfo struct {
	// ID is the raw JSON token of the request id, echoed unchanged in a reply.
	ID json.RawMessage
	// Method is the request method.
	Method string
	// Legacy reports whether the request carries no protocol metadata and is
	// therefore an initialize-handshake request, exempt from the discovery
	// rules. A method the discovery handshake introduced is never reported as
	// legacy, whatever its _meta holds: no client predating that metadata can
	// be calling it, so the exemption has nobody to speak for and the request
	// is held to the discovery rules in full (transport status mapping and
	// mirrored headers included).
	Legacy bool
	// ProtocolVersion is the params._meta protocol version.
	ProtocolVersion string
	// HasProtocolVersion reports whether the body actually states a string
	// protocol version. It distinguishes a stated empty string, which a
	// mirrored header must match, from an absent or ill-typed member, which a
	// header cannot contradict.
	HasProtocolVersion bool
	// Name is the request's named target (the tool or prompt name, or the
	// resource uri).
	Name string
	// HasName reports whether the body states a string named target, with the
	// same distinction HasProtocolVersion draws.
	HasName bool
	// RequiresName reports whether the method addresses a named target, and so
	// whether an Mcp-Name header is required for it.
	RequiresName bool
}

// InspectMessage decodes a raw inbound message far enough to describe it,
// reporting ok=false when the message is not a JSON-RPC request that carries
// both a usable id (a non-null string or number) and a string method (a
// notification, a reply, or malformed input). Callers treat a false result as
// "not mine to validate" and let the server produce the proper JSON-RPC error.
func InspectMessage(raw []byte) (MessageInfo, bool) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return MessageInfo{}, false
	}

	// A usable id is one the specification permits for a request: a non-null
	// string or number. It is the same rule the server applies before echoing an
	// id back, so a message this reports as a request is one the server will
	// answer with that very id, and nothing else is ever echoed.
	var id jsonrpc.ID
	if err := id.UnmarshalJSON(body["id"]); err != nil || !id.IsValidRequestID() {
		return MessageInfo{}, false
	}

	// The method is decoded as a free-form value and then required to be a
	// string: unmarshalling straight into a string would accept a JSON null,
	// which encoding/json turns into the empty string without error, and report
	// a message the server answers with an invalid-request error as a request
	// named "".
	var methodValue any
	if err := json.Unmarshal(body["method"], &methodValue); err != nil {
		return MessageInfo{}, false
	}
	method, isString := methodValue.(string)
	if !isString {
		return MessageInfo{}, false
	}

	var params rawMembers
	if rawParams, present := body["params"]; present {
		params, _ = objectMembers(rawParams)
	}

	meta, hasMeta := metaFromParams(params)
	info := MessageInfo{
		ID:           id.Raw(),
		Method:       method,
		Legacy:       isLegacyRequest(method, meta, hasMeta),
		RequiresName: namedParamKey(method) != "",
	}
	info.ProtocolVersion, info.HasProtocolVersion = stringMember(meta[MetaKeyProtocolVersion])
	if key := namedParamKey(method); key != "" {
		info.Name, info.HasName = stringMember(params[key])
	}
	return info, true
}

// metaFromParams pulls the _meta object out of a decoded params object. A _meta
// member that is not an object is reported as absent, matching how the rest of
// the protocol treats a malformed metadata bag.
func metaFromParams(params rawMembers) (rawMembers, bool) {
	if params == nil {
		return nil, false
	}
	raw, present := params["_meta"]
	if !present {
		return nil, false
	}
	return objectMembers(raw)
}

// isLegacyMeta reports whether a request's _meta marks it as a legacy
// initialize-handshake request. A request is legacy exactly when it declares
// neither the protocol version nor the client capabilities; declaring either
// one (even as null) makes it a discovery-handshake request, which is then
// validated in full.
func isLegacyMeta(meta rawMembers, hasMeta bool) bool {
	if !hasMeta {
		return true
	}
	if _, ok := meta[MetaKeyProtocolVersion]; ok {
		return false
	}
	_, ok := meta[MetaKeyClientCapabilities]
	return !ok
}

// isLegacyRequest reports whether a whole message falls under the legacy
// exemption: its _meta declares no protocol metadata AND its method is one an
// older client could be calling. It is the single rule every reader of the
// exemption applies, so a message cannot be held to the discovery rules by the
// method handler while a transport reads it as legacy and answers it under the
// older ones.
func isLegacyRequest(method string, meta rawMembers, hasMeta bool) bool {
	return isLegacyMeta(meta, hasMeta) && !requiresProtocolMeta(method)
}

// requestMeta decodes a request's params._meta object. The params member has
// already been checked to be an object (or the empty array that stands in for
// one) by the strict request parser, so the only shape that reaches the false
// result here is one carrying no _meta at all.
func requestMeta(req *jsonrpc.Request) (rawMembers, bool) {
	if req == nil || len(req.Params) == 0 {
		return nil, false
	}
	params, ok := objectMembers(req.Params)
	if !ok {
		return nil, false
	}
	return metaFromParams(params)
}

// validateProtocolMeta enforces the per-request protocol metadata rules of the
// discovery handshake. A legacy request (one declaring neither the protocol
// version nor the client capabilities) is exempt and validates trivially, unless
// it names a method the discovery handshake introduced: such a request cannot be
// coming from a client that predates the metadata, so an absent or ill-typed
// _meta is a malformed request rather than an older one.
//
// A discovery request must declare a string protocol version and an object of
// client capabilities; a missing or ill-typed member is CodeInvalidParams
// (-32602). A well-formed version the server does not speak is
// CodeUnsupportedProtocolVersion (-32022), carrying the supported list and the
// requested value so the client can renegotiate. That answer is also how a
// client discovers which revisions the server speaks when its opening
// server/discover names one the server does not.
func validateProtocolMeta(c *Context, req *jsonrpc.Request) *jsonrpc.Error {
	if req == nil {
		return nil
	}
	meta, hasMeta := requestMeta(req)
	if isLegacyRequest(req.Method, meta, hasMeta) {
		return nil
	}

	requested, ok := stringMember(meta[MetaKeyProtocolVersion])
	if !ok {
		return jsonrpc.NewError(jsonrpc.CodeInvalidParams, missingMetaMessage(MetaKeyProtocolVersion))
	}
	if !isObjectMember(meta[MetaKeyClientCapabilities]) {
		return jsonrpc.NewError(jsonrpc.CodeInvalidParams, missingMetaMessage(MetaKeyClientCapabilities))
	}

	supported := c.SupportedProtocolVersions()
	if !containsVersion(supported, requested) {
		return jsonrpc.NewError(jsonrpc.CodeUnsupportedProtocolVersion, msgUnsupportedVersion).
			WithData(map[string]any{
				"supported": supported,
				"requested": requested,
			})
	}
	return nil
}

// RequestProtocolVersion returns the protocol revision a request declares in
// its params._meta, and whether it declares one at all. A legacy request
// declares none, so ok is false: its revision was settled by the initialize
// handshake and is not restated per request.
//
// It is what a method handler whose answer differs between revisions reads
// before shaping that answer, so a client of an older revision keeps the
// response it was built against. By the time a handler runs, a declared version
// has already been checked against the server's supported list (see
// validateProtocolMeta), so a version reported here is one the server speaks.
func RequestProtocolVersion(req *jsonrpc.Request) (ProtocolVersion, bool) {
	meta, hasMeta := requestMeta(req)
	if !hasMeta {
		return "", false
	}
	return stringMember(meta[MetaKeyProtocolVersion])
}

// missingMetaMessage builds the invalid-params message naming the _meta member
// at fault. It avoids fmt so the hot request path does no reflection.
func missingMetaMessage(key string) string {
	const prefix = "Invalid params: The request [_meta] is missing the required ["
	return prefix + key + "] member."
}

// msgInvalidArguments is the message of the -32602 error reporting an
// [arguments] member that is not an object.
const msgInvalidArguments = "Invalid params: The [arguments] member must be an object."

// validateArguments enforces the shape of a request's [arguments] member, the
// bag of values a tool, prompt, or resource handler is invoked with. An absent
// member is valid and means no arguments; a present member that is not an
// object is CodeInvalidParams (-32602), because running the primitive with an
// empty bag would silently answer a request the client never made.
//
// Only the methods the specification gives that member are held to the rule. A
// method added through WithMethod defines its own parameters, so an [arguments]
// member it models as a list or a string is its handler's to read, not this
// check's to refuse.
func validateArguments(req *jsonrpc.Request) *jsonrpc.Error {
	if req == nil || !readsArgumentBag(req.Method) || len(req.Params) == 0 {
		return nil
	}
	params, ok := objectMembers(req.Params)
	if !ok {
		return nil
	}
	value, present := params["arguments"]
	if !present || isObjectMember(value) {
		return nil
	}
	return jsonrpc.NewError(jsonrpc.CodeInvalidParams, msgInvalidArguments)
}
