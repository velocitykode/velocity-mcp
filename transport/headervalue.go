package transport

import (
	"encoding/base64"
	"errors"
	"strings"
)

// HTTP headers carry a restricted byte range, so a value that cannot be written
// literally (one holding non-ASCII text, control characters, or leading or
// trailing whitespace) travels base64-encoded between these sentinels. The
// sentinels are unambiguous: a literal value that happens to look encoded is
// itself encoded, so decoding never mistakes it for a wrapper.
const (
	base64Prefix = "=?base64?"
	base64Suffix = "?="
)

// Failures DecodeHeaderValue reports. Each describes a header value no peer
// following the encoding could have written, so the request carrying it states
// nothing this server can compare against its body.
var (
	// ErrHeaderValueSyntax reports a value holding bytes a header field value
	// may not carry: non-ASCII text, a control character, or whitespace at
	// either edge. Every such value has a wrapped form and must travel in it.
	ErrHeaderValueSyntax = errors.New("transport: header value is not a field value")
	// ErrHeaderValueEncoding reports a sentinel-wrapped value whose payload is
	// not base64 under either padding rule, so the value it claims to state
	// cannot be recovered.
	ErrHeaderValueEncoding = errors.New("transport: header value is not base64")
)

// EncodeHeaderValue renders a value for transport in an MCP header. A value
// that is already safe to write literally is returned unchanged; anything else
// is wrapped as "=?base64?<base64>?=". Encoding is total: every input has a
// representation, and DecodeHeaderValue recovers it exactly.
func EncodeHeaderValue(value string) string {
	if !isSentinelWrapped(value) && isLiteralHeaderValue(value) {
		return value
	}
	return base64Prefix + base64.StdEncoding.EncodeToString([]byte(value)) + base64Suffix
}

// DecodeHeaderValue recovers the value an MCP header carries, or reports why the
// header states no value this server can read. A header without the sentinels is
// its own value; a sentinel-wrapped one states its value base64-encoded.
//
// The argument is the field value alone, without the optional whitespace HTTP
// permits around it, because every remaining byte is significant. A value that
// could not have been written literally is ErrHeaderValueSyntax and a wrapped
// value whose payload does not decode is ErrHeaderValueEncoding; neither is read
// as the text it happens to be. Reading it that way would let a peer state a
// name in a form the encoding does not define, and an intermediary routing on
// the header would then resolve it differently from the server reading the body.
//
// Decoding accepts an unpadded payload as well as a padded one. EncodeHeaderValue
// always pads, but a peer that trims the padding is still stating a value this
// server can read, and comparing its payload as literal text instead would
// reject the request with a mismatch it could not act on.
func DecodeHeaderValue(header string) (string, error) {
	if !isLiteralHeaderValue(header) {
		return "", ErrHeaderValueSyntax
	}
	if !isSentinelWrapped(header) {
		return header, nil
	}
	payload := header[len(base64Prefix) : len(header)-len(base64Suffix)]
	if decoded, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return string(decoded), nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(payload); err == nil {
		return string(decoded), nil
	}
	return "", ErrHeaderValueEncoding
}

// isSentinelWrapped reports whether a value is wrapped in the base64 sentinels.
// The two sentinels must not overlap, so a value has to be long enough to hold
// both of them around a (possibly empty) payload.
func isSentinelWrapped(value string) bool {
	return len(value) >= len(base64Prefix)+len(base64Suffix) &&
		strings.HasPrefix(value, base64Prefix) &&
		strings.HasSuffix(value, base64Suffix)
}

// trimOWS strips the optional whitespace a peer may write around a field value.
// RFC 9110 section 5.6.3 defines that whitespace as SP and HTAB alone, and those
// two bytes only: they are not part of the value and a well-behaved HTTP server
// has already removed them. Every other byte is significant, so a wider notion
// of whitespace (the Unicode set, which includes U+00A0, U+2028 and the ideographic
// space) would silently drop bytes the value does carry, and a header whose value
// no encoder could have written would then be read as one that mirrors the body.
func trimOWS(value string) string {
	return strings.Trim(value, " \t")
}

// isLiteralHeaderValue reports whether a value may be written into a header as
// it stands: printable ASCII throughout, with no leading or trailing space or
// tab. That is the field-value grammar of RFC 9110, which excludes control
// characters (so a value can never inject a header break) and non-ASCII bytes.
func isLiteralHeaderValue(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		b := value[i]
		visible := b >= 0x21 && b <= 0x7E
		spacing := b == ' ' || b == '\t'
		if !visible && !spacing {
			return false
		}
		// Leading and trailing whitespace would be stripped in transit, so a
		// value carrying it is not literal-safe.
		if spacing && (i == 0 || i == len(value)-1) {
			return false
		}
	}
	return true
}
