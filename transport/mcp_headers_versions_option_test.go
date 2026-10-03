package transport

import (
	"reflect"
	"testing"

	"github.com/velocitykode/velocity-mcp/server"
	"github.com/velocitykode/velocity/router"
)

// This file covers how the header validation learns which protocol versions the
// served server speaks: from the server itself when mounted through Handler,
// and from WithProtocolVersions when mounted on its own.

// TestHandlerReadsTheProtocolVersionsFromTheServer asserts a WithProtocolVersions
// handed to Handler cannot make the transport misreport: the server it serves is
// the authority on what it speaks.
func TestHandlerReadsTheProtocolVersionsFromTheServer(t *testing.T) {
	srv := server.New("versions", "1.0.0", server.WithProtocolVersions("2027-01-01"))
	c, w := postContext(t, legacyToolCall, HeaderProtocolVersion, "1900-01-01")
	if err := Handler(srv, WithProtocolVersions("1111-11-11"))(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	_, supported := unsupportedVersionData(t, headerError(t, w))
	want := []any{"2027-01-01", "2025-11-25", "2025-06-18"}
	if !reflect.DeepEqual(supported, want) {
		t.Fatalf("data.supported = %#v, want %#v", supported, want)
	}
}

// TestValidateHeadersOnItsOwnReportsTheVersionsItWasGiven asserts the middleware
// mounted without a server reports the list WithProtocolVersions supplied, and
// the package default when none was.
func TestValidateHeadersOnItsOwnReportsTheVersionsItWasGiven(t *testing.T) {
	tests := []struct {
		name string
		opts []HandlerOption
		want []any
	}{
		{"configured", []HandlerOption{WithProtocolVersions("2027-01-01")}, []any{"2027-01-01", "2025-11-25", "2025-06-18"}},
		{"default", nil, []any{"2026-07-28", "2025-11-25", "2025-06-18"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := func(c *router.Context) error {
				t.Fatal("the handler ran for a request naming a protocol version the server does not speak")
				return nil
			}
			c, w := postContext(t, legacyToolCall, HeaderProtocolVersion, "1900-01-01")
			if err := ValidateHeaders(tt.opts...)(next)(c); err != nil {
				t.Fatalf("middleware returned error: %v", err)
			}
			_, supported := unsupportedVersionData(t, headerError(t, w))
			if !reflect.DeepEqual(supported, tt.want) {
				t.Fatalf("data.supported = %#v, want %#v", supported, tt.want)
			}
		})
	}
}
