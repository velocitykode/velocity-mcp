package transport

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// This file covers what a -32022 written by the transport tells the client. The
// specification (2026-07-28, versioning, protocol version negotiation) requires
// the error's data.supported to list the versions the server does support, so
// the list has to describe the server instance being served, not the package:
// a server built with server.WithProtocolVersions speaks a different set, and a
// client that retried with a version listed here only to be refused again would
// have been told two different things by one server.

// legacyList is an initialize-era tools/list, a request every server can
// answer whatever primitives it registers.
const legacyList = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

// unsupportedVersionData decodes the data member of a -32022 reply, failing the
// test on any other reply.
func unsupportedVersionData(t *testing.T, resp jsonrpc.Response) (requested string, supported []any) {
	t.Helper()
	if resp.Error == nil || resp.Error.Code != jsonrpc.CodeUnsupportedProtocolVersion {
		t.Fatalf("error = %+v, want code %d", resp.Error, jsonrpc.CodeUnsupportedProtocolVersion)
	}
	data, ok := resp.Error.Data.(map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want an object", resp.Error.Data)
	}
	requested, _ = data["requested"].(string)
	supported, _ = data["supported"].([]any)
	return requested, supported
}

// TestUnsupportedVersionHeaderListsTheServedServersVersions asserts the
// transport reports the versions the server instance was configured with, plus
// the ones the initialize handshake negotiates for every server, and nothing
// else: in particular not the package default the server was configured away
// from.
func TestUnsupportedVersionHeaderListsTheServedServersVersions(t *testing.T) {
	tests := []struct {
		name       string
		configured []server.ProtocolVersion
		header     string
		want       []any
	}{
		{
			name:       "a server speaking another revision",
			configured: []server.ProtocolVersion{"2027-01-01"},
			header:     "1900-01-01",
			want:       []any{"2027-01-01", "2025-11-25", "2025-06-18"},
		},
		{
			name:       "the package default named by a server configured away from it",
			configured: []server.ProtocolVersion{"2027-01-01"},
			header:     "2026-07-28",
			want:       []any{"2027-01-01", "2025-11-25", "2025-06-18"},
		},
		{
			name:       "a server accepting an initialize revision in its metadata too lists it once",
			configured: []server.ProtocolVersion{"2025-11-25", "2026-07-28"},
			header:     "1900-01-01",
			want:       []any{"2025-11-25", "2026-07-28", "2025-06-18"},
		},
		{
			name:       "a server with the default list",
			configured: nil,
			header:     "1900-01-01",
			want:       []any{"2026-07-28", "2025-11-25", "2025-06-18"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := server.New("versions", "1.0.0", server.WithProtocolVersions(tt.configured...))
			resp := headerError(t, serveWith(t, srv, legacyToolCall, HeaderProtocolVersion, tt.header))
			requested, supported := unsupportedVersionData(t, resp)
			if requested != tt.header {
				t.Fatalf("data.requested = %q, want %q", requested, tt.header)
			}
			if !reflect.DeepEqual(supported, tt.want) {
				t.Fatalf("data.supported = %#v, want %#v", supported, tt.want)
			}
		})
	}
}

// TestEveryVersionTheTransportAdvertisesIsThenAccepted asserts the list is
// truthful end to end: a client that restates any version a -32022 named is
// served, whether it does so as a discovery request declaring the version in
// its metadata or as an initialize-era request stating it in the header alone.
func TestEveryVersionTheTransportAdvertisesIsThenAccepted(t *testing.T) {
	srv := server.New("versions", "1.0.0", server.WithProtocolVersions("2027-01-01"))
	_, supported := unsupportedVersionData(t, headerError(t, serveWith(t, srv, legacyToolCall, HeaderProtocolVersion, "1900-01-01")))
	if len(supported) == 0 {
		t.Fatal("the refusal advertised no versions at all")
	}

	for _, item := range supported {
		version, _ := item.(string)
		t.Run(version, func(t *testing.T) {
			body, headers := legacyList, []string{HeaderProtocolVersion, version}
			if version == "2027-01-01" {
				body = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
					`"io.modelcontextprotocol/protocolVersion":"` + version + `",` +
					`"io.modelcontextprotocol/clientCapabilities":{}}}}`
				headers = append(headers, HeaderMethod, "tools/list")
			}
			w := serveWith(t, srv, body, headers...)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if resp := decodeResponse(t, w.Body.Bytes()); resp.Error != nil {
				t.Fatalf("a version the transport advertised was refused: %+v", resp.Error)
			}
		})
	}
}

// TestInitializeRevisionKeepsTheExemptionWhenAlsoAcceptedInMetadata asserts a
// server configured to accept an initialize-era revision in a request's metadata
// as well still serves the older client that states that revision in its header
// and nothing in its body: the exemption speaks for that client first.
func TestInitializeRevisionKeepsTheExemptionWhenAlsoAcceptedInMetadata(t *testing.T) {
	srv := server.New("versions", "1.0.0", server.WithProtocolVersions("2025-11-25", "2026-07-28"))
	w := serveWith(t, srv, legacyList, HeaderProtocolVersion, "2025-11-25")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if resp := decodeResponse(t, w.Body.Bytes()); resp.Error != nil {
		t.Fatalf("legacy request refused: %+v", resp.Error)
	}
	if !strings.Contains(w.Body.String(), `"tools"`) {
		t.Fatalf("the list did not run: %s", w.Body.String())
	}
}

// metaList builds a tools/list declaring version in its protocol metadata.
func metaList(version string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"` + version + `",` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
}

// TestBothRefusalsOfAVersionReportTheSameList asserts one server tells a client
// one thing. A version can be refused by the transport, from the header of a
// request whose body declares nothing, or by the server, from the metadata of a
// request whose header agrees with it; either way data.supported is the same
// list, so a version a client picks from one answer is not then missing from
// the other.
func TestBothRefusalsOfAVersionReportTheSameList(t *testing.T) {
	tests := []struct {
		name       string
		configured []server.ProtocolVersion
		want       []any
	}{
		{"a server with the default list", nil, []any{"2026-07-28", "2025-11-25", "2025-06-18"}},
		{"a server speaking another revision", []server.ProtocolVersion{"2027-01-01"}, []any{"2027-01-01", "2025-11-25", "2025-06-18"}},
		{"a server accepting an initialize revision in its metadata too", []server.ProtocolVersion{"2025-11-25", "2026-07-28"}, []any{"2025-11-25", "2026-07-28", "2025-06-18"}},
	}
	const refused = "1900-01-01"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := server.New("versions", "1.0.0", server.WithProtocolVersions(tt.configured...))

			_, fromHeader := unsupportedVersionData(t, headerError(t, serveWith(t, srv, legacyList, HeaderProtocolVersion, refused)))
			_, fromBody := unsupportedVersionData(t, headerError(t, serveWith(t, srv, metaList(refused), HeaderProtocolVersion, refused, HeaderMethod, "tools/list")))

			if !reflect.DeepEqual(fromHeader, tt.want) {
				t.Fatalf("refused from the header: data.supported = %#v, want %#v", fromHeader, tt.want)
			}
			if !reflect.DeepEqual(fromBody, tt.want) {
				t.Fatalf("refused from the body: data.supported = %#v, want %#v", fromBody, tt.want)
			}
		})
	}
}

// TestAHandshakeRevisionDeclaredInMetadataIsAnsweredAsMisplaced asserts a
// client that reads an initialize-era revision out of a -32022 and restates it
// in a request's metadata, rather than opening with initialize, is told that
// with HTTP 400 and -32602. A second -32022 would list the very version it
// refused.
func TestAHandshakeRevisionDeclaredInMetadataIsAnsweredAsMisplaced(t *testing.T) {
	for _, version := range server.InitializeSupportedVersions() {
		t.Run(version, func(t *testing.T) {
			srv := server.New("versions", "1.0.0")
			resp := headerError(t, serveWith(t, srv, metaList(version), HeaderProtocolVersion, version, HeaderMethod, "tools/list"))
			if resp.Error == nil || resp.Error.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("error = %+v, want code %d", resp.Error, jsonrpc.CodeInvalidParams)
			}
			if !strings.Contains(resp.Error.Message, version) || !strings.Contains(resp.Error.Message, "initialize") {
				t.Fatalf("message %q does not name the version and the handshake that negotiates it", resp.Error.Message)
			}
		})
	}
}
