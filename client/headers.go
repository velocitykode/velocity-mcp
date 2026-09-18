package client

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Request headers carried by a discovery-era exchange. HTTP header names are
// case insensitive; these are the canonical spellings.
const (
	// protocolVersionHeader advertises the protocol version of the exchange.
	protocolVersionHeader = "Mcp-Protocol-Version"
	// methodHeader mirrors the JSON-RPC method of the request body so an
	// intermediary can route or authorize the call without parsing the body.
	methodHeader = "Mcp-Method"
	// nameHeader mirrors the name of the primitive the request addresses.
	nameHeader = "Mcp-Name"
	// sessionHeader carries the session id of an initialize-era exchange.
	sessionHeader = "Mcp-Session-Id"
)

// Keys of the protocol metadata carried in the _meta member of a discovery-era
// request or result.
const (
	// MetaProtocolVersion is the protocol version of the request.
	MetaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	// MetaClientCapabilities is the capability set of the calling client.
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	// MetaClientInfo is the identity of the calling client.
	MetaClientInfo = "io.modelcontextprotocol/clientInfo"
	// MetaServerInfo is the identity of the server, returned in a discover
	// result.
	MetaServerInfo = "io.modelcontextprotocol/serverInfo"
)

// base64 header form markers. A value that cannot travel verbatim in an HTTP
// header is wrapped in them, so the receiver can tell an encoded value from a
// literal one.
const (
	base64HeaderPrefix = "=?base64?"
	base64HeaderSuffix = "?="
)

// mirroredHeaders returns the headers that mirror a request frame: always the
// method, plus the name of the addressed primitive when the method names one.
// params is the encoded params member of the request.
func mirroredHeaders(method string, params json.RawMessage) map[string]string {
	headers := map[string]string{methodHeader: method}
	if name, ok := mirroredName(method, params); ok {
		headers[nameHeader] = encodeHeaderValue(name)
	}
	return headers
}

// mirroredName extracts the name of the primitive a request addresses, or
// reports false when the method addresses none (or the member is not a string).
func mirroredName(method string, params json.RawMessage) (string, bool) {
	key := nameKeyFor(method)
	if key == "" || len(params) == 0 {
		return "", false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(params, &members); err != nil {
		return "", false
	}
	raw, ok := members[key]
	if !ok {
		return "", false
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", false
	}
	return name, true
}

// nameKeyFor returns the params member holding the name of the primitive a
// method addresses, or the empty string for a method that addresses none.
func nameKeyFor(method string) string {
	switch method {
	case "tools/call", "prompts/get":
		return "name"
	case "resources/read":
		return "uri"
	default:
		return ""
	}
}

// encodeHeaderValue renders a value for an HTTP header field. A value made of
// printable US-ASCII travels verbatim; anything else (an empty value, one
// carrying unicode, control characters, or leading or trailing whitespace, or
// one that already looks encoded) travels base64 encoded between the =?base64?
// and ?= markers, so no value can inject a header break or be mistaken for a
// literal.
func encodeHeaderValue(value string) string {
	if !isBase64HeaderValue(value) && isPrintableFieldValue(value) {
		return value
	}
	return base64HeaderPrefix + base64.StdEncoding.EncodeToString([]byte(value)) + base64HeaderSuffix
}

// isBase64HeaderValue reports whether a value already carries the base64 header
// markers.
func isBase64HeaderValue(value string) bool {
	return strings.HasPrefix(value, base64HeaderPrefix) && strings.HasSuffix(value, base64HeaderSuffix)
}

// isPrintableFieldValue reports whether a value is a valid HTTP field value
// that needs no encoding: a non-empty run of printable US-ASCII whose first and
// last bytes are not whitespace, with spaces and tabs allowed in between.
func isPrintableFieldValue(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		edge := index == 0 || index == len(value)-1
		switch {
		case char >= 0x21 && char <= 0x7E:
		case !edge && (char == ' ' || char == '\t'):
		default:
			return false
		}
	}
	return true
}
