package transport

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEncodeHeaderValue pins which values travel literally and which are
// wrapped. Anything a header cannot carry byte for byte must be wrapped, or the
// value silently changes in transit and the mirroring check compares the wrong
// thing.
func TestEncodeHeaderValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"plain token", "us-west1", "us-west1"},
		{"method name", "tools/call", "tools/call"},
		{"uri", "file:///projects/myapp/config.json", "file:///projects/myapp/config.json"},
		{"inner spaces are fine", "say hi", "say hi"},
		{"non-ascii", "Hello, 世界", "=?base64?SGVsbG8sIOS4lueVjA==?="},
		{"leading and trailing space", " padded ", "=?base64?IHBhZGRlZCA=?="},
		{"newline", "line1\nline2", "=?base64?bGluZTEKbGluZTI=?="},
		{"carriage return", "a\rb", "=?base64?YQ1i?="},
		{"tab at the edge", "\tx", "=?base64?CXg=?="},
		{"looks like a wrapper", "=?base64?literal?=", "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?="},
		{"empty", "", "=?base64??="},
		{"nul byte", "a\x00b", "=?base64?YQBi?="},
		{"delete character", "a\x7fb", "=?base64?YX9i?="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EncodeHeaderValue(tt.value); got != tt.want {
				t.Fatalf("EncodeHeaderValue(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// TestDecodeHeaderValue pins what a header states and what it cannot state. A
// value the encoding does not define is a decoding failure, never the literal
// text it happens to be: read as text it would let a peer name a primitive in a
// spelling no encoder produces.
func TestDecodeHeaderValue(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		want    string
		wantErr error
	}{
		{name: "plain token", header: "tools/call", want: "tools/call"},
		{name: "wrapped", header: "=?base64?SGVsbG8sIOS4lueVjA==?=", want: "Hello, 世界"},
		{name: "empty payload", header: "=?base64??=", want: ""},
		{name: "prefix without suffix", header: "=?base64?abc", want: "=?base64?abc"},
		{name: "suffix without prefix", header: "abc?=", want: "abc?="},
		{name: "overlapping sentinels", header: "=?base64?=", want: "=?base64?="},
		{name: "undecodable payload", header: "=?base64?not base64!?=", wantErr: ErrHeaderValueEncoding},
		{name: "sentinel-shaped junk", header: "=?base64?%%%?=", wantErr: ErrHeaderValueEncoding},
		{name: "payload with an interior newline", header: "=?base64?ab\ncd?=", wantErr: ErrHeaderValueSyntax},
		{name: "raw non-ascii", header: "Hello, 世界", wantErr: ErrHeaderValueSyntax},
		{name: "control character", header: "a\x00b", wantErr: ErrHeaderValueSyntax},
		{name: "delete character", header: "a\x7fb", wantErr: ErrHeaderValueSyntax},
		{name: "leading space", header: " padded", wantErr: ErrHeaderValueSyntax},
		{name: "trailing tab", header: "padded\t", wantErr: ErrHeaderValueSyntax},
		{name: "empty header", header: "", wantErr: ErrHeaderValueSyntax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeHeaderValue(tt.header)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("DecodeHeaderValue(%q) error = %v, want %v", tt.header, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if got != "" {
					t.Fatalf("a refused header yielded the value %q", got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("DecodeHeaderValue(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

// TestHeaderValueRoundTrip asserts encoding is lossless for every value the
// protocol may carry, including the awkward ones.
func TestHeaderValueRoundTrip(t *testing.T) {
	values := []string{
		"us-west1",
		"tools/call",
		"Hello, 世界",
		" padded ",
		"line1\nline2",
		"=?base64?literal?=",
		"file:///projects/myapp/config.json",
		"",
		"a\x00b",
		"\u202eevil",
		"emoji \U0001F600",
	}
	for _, value := range values {
		got, err := DecodeHeaderValue(EncodeHeaderValue(value))
		if err != nil {
			t.Fatalf("encoding of %q is not decodable: %v", value, err)
		}
		if got != value {
			t.Fatalf("round trip of %q produced %q", value, got)
		}
	}
}

// TestEncodedValueIsAcceptedByNetHTTP asserts an encoded value can actually be
// written to a request header: a value that Go refuses to send would make the
// mirroring requirement impossible to satisfy for those names.
func TestEncodedValueIsAcceptedByNetHTTP(t *testing.T) {
	values := []string{"Hello, 世界", " padded ", "line1\nline2", "a\x00b", ""}
	for _, value := range values {
		encoded := EncodeHeaderValue(value)
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set(HeaderName, encoded)
		rec := httptest.NewRecorder()
		if err := req.Header.Write(rec.Body); err != nil {
			t.Fatalf("header value %q (from %q) is not writable: %v", encoded, value, err)
		}
		if got := req.Header.Get(HeaderName); got != encoded {
			t.Fatalf("header round trip of %q gave %q", encoded, got)
		}
	}
}

// FuzzDecodeHeaderValue drives the header decoder, which reads untrusted input
// straight off the wire. It must never panic, must never invent a value for a
// header that is not a well-formed wrapper, and must accept exactly the values
// an encoder could have written.
func FuzzDecodeHeaderValue(f *testing.F) {
	seeds := []string{
		"", "tools/call", "=?base64??=", "=?base64?SGVsbG8=?=", "=?base64?not base64!?=",
		"=?base64?=", "=?base64", "?=", "=?base64?////?=", "=?base64?" + string([]byte{0xff}) + "?=",
		"=?base64?%%%?=", "=?base64?SGVsbG8?=", "=?base64?=?base64??=?=", " tools/call",
		"tools/call ", "Hello, 世界", "a\x00b", "a\rb", "=?base64?ab\ncd?=", "=?base64?SGVsbG8=?=?=",
		"\u00a0tools/call", "tools/call\u3000", "\u00a0=?base64?SGVsbG8=?=",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		decoded, err := DecodeHeaderValue(header)
		payload, wrapped := sentinelPayload(header)
		spelled, spelledErr := decodeBase64Payload(payload)

		if err != nil {
			if decoded != "" {
				t.Fatalf("refused header %q still yielded %q", header, decoded)
			}
			// A refusal holds only where no peer following the encoding could
			// have written the header. Two forms could: a value the field-value
			// grammar lets stand as it is, which travels unchanged unless it
			// wears the sentinels itself, and a wrapped payload that is base64.
			// Both are settled from the grammar and the standard library, not
			// from the encoder, so the property does not ask the code under
			// test whether its own refusal was right.
			if writableAsItStands(header) && !wrapped {
				t.Fatalf("refused %q, which a peer may write as it stands", header)
			}
			if writableAsItStands(header) && wrapped && spelledErr == nil {
				t.Fatalf("refused %q, whose payload is the base64 of %q", header, spelled)
			}
			return
		}

		// An accepted header states exactly one value: the payload it wraps, or
		// itself when it wraps nothing.
		switch {
		case wrapped:
			if spelledErr != nil {
				t.Fatalf("accepted %q, whose payload is not base64: %v", header, spelledErr)
			}
			if decoded != spelled {
				t.Fatalf("header %q decoded to %q, want %q", header, decoded, spelled)
			}
		case decoded != header:
			t.Fatalf("unwrapped header %q decoded to %q", header, decoded)
		}

		// Encoding whatever came out must round trip: the pair is total.
		got, rerr := DecodeHeaderValue(EncodeHeaderValue(decoded))
		if rerr != nil {
			t.Fatalf("re-encoding of decoded %q is not decodable: %v", decoded, rerr)
		}
		if got != decoded {
			t.Fatalf("round trip of decoded %q produced %q", decoded, got)
		}
	})
}

// writableAsItStands reports whether a value may be written into a header field
// as it stands. It states the field-value grammar of RFC 9110 section 5.6.3
// directly (visible ASCII, with SP and HTAB permitted only inside the value),
// so the properties above rest on the grammar rather than on the predicate the
// code under test uses.
func writableAsItStands(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		switch b := value[i]; {
		case b == ' ' || b == '\t':
			if i == 0 || i == len(value)-1 {
				return false
			}
		case b < 0x21 || b > 0x7E:
			return false
		}
	}
	return true
}

// sentinelPayload returns the payload a sentinel-wrapped header carries, and
// whether the header is wrapped at all. The sentinels must not overlap, so a
// wrapped header is long enough to hold both around a payload that may be
// empty.
func sentinelPayload(header string) (string, bool) {
	if len(header) < len(base64Prefix)+len(base64Suffix) {
		return "", false
	}
	payload, ok := strings.CutPrefix(header, base64Prefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(payload, base64Suffix)
}

// decodeBase64Payload decodes a wrapped payload with the standard library under
// both padding rules, so what a header spells is settled independently of the
// decoder under test.
func decodeBase64Payload(payload string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err == nil {
		return string(decoded), nil
	}
	raw, rawErr := base64.RawStdEncoding.DecodeString(payload)
	if rawErr == nil {
		return string(raw), nil
	}
	return "", err
}

// FuzzEncodeHeaderValue asserts every encoding is header-safe and recoverable,
// whatever bytes an application puts in a tool name or a resource uri.
func FuzzEncodeHeaderValue(f *testing.F) {
	for _, seed := range []string{"", "a", "tools/call", "Hello, 世界", " x ", "\n", "\x00", "=?base64?x?="} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		encoded := EncodeHeaderValue(value)
		for i := range len(encoded) {
			if b := encoded[i]; b < 0x21 || b > 0x7E {
				if b != ' ' && b != '\t' {
					t.Fatalf("EncodeHeaderValue(%q) = %q, byte %d is not header-safe", value, encoded, i)
				}
			}
		}
		got, err := DecodeHeaderValue(encoded)
		if err != nil {
			t.Fatalf("EncodeHeaderValue(%q) = %q, which does not decode: %v", value, encoded, err)
		}
		if got != value {
			t.Fatalf("round trip of %q produced %q", value, got)
		}
	})
}
