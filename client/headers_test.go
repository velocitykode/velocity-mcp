package client

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeHeaderValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "a printable token travels verbatim", value: "execute_sql", want: "execute_sql"},
		{name: "inner spaces travel verbatim", value: "Us West 1", want: "Us West 1"},
		{name: "an inner tab travels verbatim", value: "a\tb", want: "a\tb"},
		{name: "a uri travels verbatim", value: "file:///notes.txt", want: "file:///notes.txt"},
		{name: "a leading space is encoded", value: " leading", want: "=?base64?IGxlYWRpbmc=?="},
		{name: "a trailing space is encoded", value: "trailing ", want: "=?base64?dHJhaWxpbmcg?="},
		{name: "an empty value is encoded", value: "", want: "=?base64??="},
		{name: "unicode is encoded", value: "café", want: "=?base64?Y2Fmw6k=?="},
		{name: "a newline is encoded", value: "a\nb", want: "=?base64?YQpi?="},
		{name: "a carriage return is encoded", value: "a\rb", want: "=?base64?YQ1i?="},
		{name: "a null byte is encoded", value: "a\x00b", want: "=?base64?YQBi?="},
		{name: "a delete byte is encoded", value: "a\x7fb", want: "=?base64?YX9i?="},
		{
			name:  "a value that already looks encoded is encoded again",
			value: "=?base64?Y2Fmw6k=?=",
			want:  "=?base64?PT9iYXNlNjQ/WTJGbXc2az0/PQ==?=",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeHeaderValue(tc.value); got != tc.want {
				t.Fatalf("encodeHeaderValue(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestMirroredHeaders(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params string
		want   map[string]string
	}{
		{
			name:   "a tool call mirrors its name",
			method: "tools/call",
			params: `{"name":"add","arguments":{"a":1}}`,
			want:   map[string]string{methodHeader: "tools/call", nameHeader: "add"},
		},
		{
			name:   "a prompt fetch mirrors its name",
			method: "prompts/get",
			params: `{"name":"greeting"}`,
			want:   map[string]string{methodHeader: "prompts/get", nameHeader: "greeting"},
		},
		{
			name:   "a resource read mirrors its uri",
			method: "resources/read",
			params: `{"uri":"file:///notes.txt"}`,
			want:   map[string]string{methodHeader: "resources/read", nameHeader: "file:///notes.txt"},
		},
		{
			name:   "a listing mirrors the method alone",
			method: "tools/list",
			params: `{"cursor":"page-2"}`,
			want:   map[string]string{methodHeader: "tools/list"},
		},
		{
			name:   "a method with no params mirrors the method alone",
			method: "ping",
			params: "",
			want:   map[string]string{methodHeader: "ping"},
		},
		{
			name:   "a missing name member is not mirrored",
			method: "tools/call",
			params: `{"arguments":{}}`,
			want:   map[string]string{methodHeader: "tools/call"},
		},
		{
			name:   "a name of the wrong type is not mirrored",
			method: "tools/call",
			params: `{"name":42}`,
			want:   map[string]string{methodHeader: "tools/call"},
		},
		{
			name:   "params that are not an object are not mirrored",
			method: "tools/call",
			params: `["add"]`,
			want:   map[string]string{methodHeader: "tools/call"},
		},
		{
			name:   "an empty name is encoded rather than dropped",
			method: "tools/call",
			params: `{"name":""}`,
			want:   map[string]string{methodHeader: "tools/call", nameHeader: "=?base64??="},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mirroredHeaders(tc.method, json.RawMessage(tc.params))
			if len(got) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", got, tc.want)
			}
			for name, want := range tc.want {
				if got[name] != want {
					t.Fatalf("header %s = %q, want %q", name, got[name], want)
				}
			}
		})
	}
}

// FuzzEncodeHeaderValue checks the property the encoding exists for: whatever a
// server or a caller puts in a name, the header value that goes on the wire is
// printable US-ASCII with no leading or trailing whitespace (so it cannot break
// the header field or inject another one), and it still carries the original
// bytes.
func FuzzEncodeHeaderValue(f *testing.F) {
	for _, seed := range []string{
		"", "add", "Us West 1", " leading", "trailing ", "café", "a\r\nX-Injected: 1",
		"a\x00b", "\x7f", "=?base64?YQ==?=", strings.Repeat("x", 300), "\t", "🙂",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		encoded := encodeHeaderValue(value)

		if !isPrintableFieldValue(encoded) {
			t.Fatalf("encodeHeaderValue(%q) = %q, which is not a usable header value", value, encoded)
		}
		if encoded == value {
			return
		}
		if !isBase64HeaderValue(encoded) {
			t.Fatalf("encodeHeaderValue(%q) = %q, which is neither the value nor its encoded form", value, encoded)
		}
		payload := strings.TrimSuffix(strings.TrimPrefix(encoded, base64HeaderPrefix), base64HeaderSuffix)
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("encoded value %q does not decode: %v", encoded, err)
		}
		if string(decoded) != value {
			t.Fatalf("encoded value %q decodes to %q, want %q", encoded, decoded, value)
		}
	})
}
