package oauth

import (
	"strings"
	"testing"

	serveroauth "github.com/velocitykode/velocity-mcp/server/oauth"
)

// A challenge is a list of auth-params whose values are tokens or
// quoted-strings (RFC 9110 11.2), and inside a quoted-string a backslash escapes
// the character after it (RFC 9110 5.6.4). A server that carries text it does
// not control in one of those values, a realm or an error description, escapes
// it exactly that way, so a parser that stops at the first quote it sees lets
// the rest of that text be read as parameters of its own: the metadata URL of a
// flow would then be whatever the text said.
func TestParseChallengeReadsQuotedStrings(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   Challenge
	}{
		{
			name:   "an escaped quote inside a value",
			header: `Bearer error="invalid_token", error_description="the token \"abc\" expired"`,
			want:   Challenge{Error: "invalid_token", ErrorDescription: `the token "abc" expired`},
		},
		{
			name:   "an escaped backslash inside a value",
			header: `Bearer error_description="C:\\temp\\token"`,
			want:   Challenge{ErrorDescription: `C:\temp\token`},
		},
		{
			name:   "an escaped character that needed no escaping",
			header: `Bearer scope="mcp\:use"`,
			want:   Challenge{Scope: "mcp:use"},
		},
		{
			name:   "escaped text that spells a parameter, ahead of the real one",
			header: `Bearer realm="x\", resource_metadata=\"https://evil.example/prm", resource_metadata="https://mcp.example.com/prm"`,
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm"},
		},
		{
			name:   "escaped text that spells a parameter, after the real one",
			header: `Bearer resource_metadata="https://mcp.example.com/prm", realm="x\", resource_metadata=\"https://evil.example/prm"`,
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm"},
		},
		{
			name:   "escaped text that spells a scope",
			header: `Bearer scope="files:read", error_description="denied\", scope=\"admin"`,
			want:   Challenge{Scope: "files:read", ErrorDescription: `denied", scope="admin`},
		},
		{
			name:   "a comma and an equals sign inside a value",
			header: `Bearer realm="a, scope=admin", scope="files:read"`,
			want:   Challenge{Scope: "files:read"},
		},
		{
			name:   "whitespace around the equals sign",
			header: "Bearer resource_metadata \t= \t\"https://mcp.example.com/prm\" ,scope = files:read",
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm", Scope: "files:read"},
		},
		{
			name:   "parameter names in another case",
			header: `bearer Resource_Metadata="https://mcp.example.com/prm", SCOPE="files:read"`,
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm", Scope: "files:read"},
		},
		{
			name:   "a parameter stated twice keeps its first value",
			header: `Bearer resource_metadata="https://mcp.example.com/prm", resource_metadata="https://evil.example/prm"`,
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm"},
		},
		{
			name:   "the bearer challenge among several",
			header: `Basic realm="staff", Bearer resource_metadata="https://mcp.example.com/prm", scope="files:read", DPoP algs="ES256", scope="other"`,
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm", Scope: "files:read"},
		},
		{
			name:   "a token68 credential ahead of the bearer challenge",
			header: `Negotiate YWJjZGVm==, Bearer scope="files:read"`,
			want:   Challenge{Scope: "files:read"},
		},
		{
			name:   "parameters of a challenge that is not a bearer one",
			header: `DPoP resource_metadata="https://mcp.example.com/prm", scope="files:read"`,
			want:   Challenge{ResourceMetadataURL: "https://mcp.example.com/prm", Scope: "files:read"},
		},
		{
			name:   "a value whose quoted-string never ends",
			header: `Bearer scope="files:read", resource_metadata="https://mcp.example.com/prm`,
			want:   Challenge{Scope: "files:read"},
		},
		{
			name:   "a value that ends in a lone backslash",
			header: `Bearer scope="files:read", error_description="oops\`,
			want:   Challenge{Scope: "files:read"},
		},
		{
			name:   "an empty quoted value",
			header: `Bearer scope="", error="invalid_token"`,
			want:   Challenge{Error: "invalid_token"},
		},
		{name: "a scheme and nothing else", header: `Bearer`, want: Challenge{}},
		{name: "separators and nothing else", header: ` , ,,  `, want: Challenge{}},
		{name: "an equals sign with no name", header: `Bearer ="x", scope="files:read"`, want: Challenge{Scope: "files:read"}},
		{name: "a name with no value", header: `Bearer scope=, error="invalid_token"`, want: Challenge{Error: "invalid_token"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseChallenge(tt.header); *got != tt.want {
				t.Fatalf("ParseChallenge(%q)\n got %+v\nwant %+v", tt.header, *got, tt.want)
			}
		})
	}
}

// What the server side of this module puts in a challenge has to come back out
// of the client side unchanged, whatever the application configured: the two are
// the ends of one flow. The server escapes quotes and backslashes and drops the
// control characters a header cannot carry.
func TestParseChallengeReadsWhatTheServerWrites(t *testing.T) {
	tests := []struct {
		name         string
		metadataURL  string
		scope        string
		wantMetadata string
		wantScope    string
	}{
		{name: "plain values", metadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource/mcp", scope: "mcp:use"},
		{name: "several scopes", metadataURL: "https://mcp.example.com/prm", scope: "files:read files:write"},
		{name: "a quote in the metadata url", metadataURL: `https://mcp.example.com/a"b`, scope: "mcp:use"},
		{name: "a backslash in the metadata url", metadataURL: `https://mcp.example.com/a\b`, scope: "mcp:use"},
		{name: "text that spells a parameter", metadataURL: `https://mcp.example.com/x", scope="admin`, scope: "mcp:use"},
		{name: "a backslash ahead of a quote", metadataURL: `https://mcp.example.com/x\", scope="admin`, scope: "mcp:use"},
		{name: "a quote in the scope", metadataURL: "https://mcp.example.com/prm", scope: `a"b`},
		{
			name:         "control characters, which the server drops",
			metadataURL:  "https://mcp.example.com/p\r\nrm",
			scope:        "mcp\x00:use",
			wantMetadata: "https://mcp.example.com/prm",
			wantScope:    "mcp:use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := serveroauth.ChallengeValue(tt.metadataURL, tt.scope)
			wantMetadata, wantScope := tt.metadataURL, tt.scope
			if tt.wantMetadata != "" {
				wantMetadata, wantScope = tt.wantMetadata, tt.wantScope
			}

			got := ParseChallenge(header)
			if got.ResourceMetadataURL != wantMetadata || got.Scope != wantScope {
				t.Fatalf("ParseChallenge(%q)\n got metadata %q scope %q\nwant metadata %q scope %q",
					header, got.ResourceMetadataURL, got.Scope, wantMetadata, wantScope)
			}
		})
	}
}

// FuzzParseChallenge feeds the parser a header the way a server would build one
// from values it does not control. Whatever those values are, they come back as
// they went in (less the control characters no header carries), and nothing in
// one value is ever read as another parameter. The quoting here is written from
// RFC 9110 5.6.4 rather than borrowed from the parser.
func FuzzParseChallenge(f *testing.F) {
	for _, seed := range [][2]string{
		{"https://mcp.example.com/prm", "mcp:use"},
		{`x", resource_metadata="https://evil.example/prm`, "mcp:use"},
		{`x\", resource_metadata=\"https://evil.example/prm`, `a", scope="admin`},
		{`\`, `\\`},
		{`"`, `""`},
		{"", ""},
		{",", "="},
		{"a=b, c=d", "Bearer scope=admin"},
		{"caf\u00e9 \u2603", "\x7f\x00\r\n\t"},
		{strings.Repeat(`\"`, 300), strings.Repeat(",", 300)},
	} {
		f.Add(seed[0], seed[1])
	}

	quote := func(v string) string {
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			switch ch := v[i]; {
			case ch == '"' || ch == '\\':
				b.WriteByte('\\')
				b.WriteByte(ch)
			case ch < 0x20 || ch == 0x7f:
			default:
				b.WriteByte(ch)
			}
		}
		return b.String()
	}
	printable := func(v string) string {
		var b strings.Builder
		for i := 0; i < len(v); i++ {
			if ch := v[i]; ch >= 0x20 && ch != 0x7f {
				b.WriteByte(ch)
			}
		}
		return b.String()
	}

	f.Fuzz(func(t *testing.T, realm, description string) {
		header := `Bearer realm="` + quote(realm) + `", resource_metadata="https://mcp.example.com/prm", error_description="` + quote(description) + `", scope="files:read"`
		want := Challenge{ResourceMetadataURL: "https://mcp.example.com/prm", ErrorDescription: printable(description), Scope: "files:read"}
		if got := ParseChallenge(header); *got != want {
			t.Fatalf("ParseChallenge(%q)\n got %+v\nwant %+v", header, *got, want)
		}
		// Anything at all must parse without a panic.
		ParseChallenge(realm)
		ParseChallenge(realm + "=" + description)
	})
}
