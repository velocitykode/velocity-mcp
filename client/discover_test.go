package client

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParseDiscoverResult(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		// wantErr is a substring of the error the payload must be refused with.
		wantErr          string
		wantVersions     []string
		wantInstructions string
		wantServerName   string
	}{
		{
			name: "a complete result",
			payload: `{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},
				"instructions":"Be nice.","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"S","version":"1.0.0"}}}`,
			wantVersions:     []string{"2026-07-28"},
			wantInstructions: "Be nice.",
			wantServerName:   "S",
		},
		{
			name:         "an empty version list is a valid result with nothing to negotiate",
			payload:      `{"supportedVersions":[],"capabilities":{}}`,
			wantVersions: []string{},
		},
		{
			name:    "an empty result is refused",
			payload: `{}`,
			wantErr: "invalid discover response from server",
		},
		{
			name:    "a result without capabilities is refused",
			payload: `{"supportedVersions":["2026-07-28"]}`,
			wantErr: "invalid discover response from server",
		},
		{
			name:    "a null version list is refused",
			payload: `{"supportedVersions":null,"capabilities":{}}`,
			wantErr: "invalid discover response from server",
		},
		{
			name:    "a version list of the wrong type is refused",
			payload: `{"supportedVersions":"2026-07-28","capabilities":{}}`,
			wantErr: "invalid discover response from server",
		},
		{
			name:    "capabilities of the wrong type are refused",
			payload: `{"supportedVersions":[],"capabilities":[]}`,
			wantErr: "invalid discover response from server",
		},
		{
			name:    "a result that is not an object is refused",
			payload: `["2026-07-28"]`,
			wantErr: "invalid discover response from server",
		},
		{
			name:         "entries that are not versions are ignored",
			payload:      `{"supportedVersions":[1,null,{"v":"x"},"2026-07-28",true],"capabilities":{}}`,
			wantVersions: []string{"2026-07-28"},
		},
		{
			name:         "a repeated member takes its last value",
			payload:      `{"capabilities":{},"supportedVersions":["2000-01-01"],"supportedVersions":["2026-07-28"]}`,
			wantVersions: []string{"2026-07-28"},
		},
		{
			name:             "instructions of the wrong type are read as none",
			payload:          `{"supportedVersions":[],"capabilities":{},"instructions":42}`,
			wantVersions:     []string{},
			wantInstructions: "",
		},
		{
			name:         "metadata of the wrong type is read as no server identity",
			payload:      `{"supportedVersions":[],"capabilities":{},"_meta":[1,2]}`,
			wantVersions: []string{},
		},
		{
			name: "a server identity without a version is read as none",
			payload: `{"supportedVersions":[],"capabilities":{},
				"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"S"}}}`,
			wantVersions: []string{},
		},
		{
			name:             "control characters and unicode in the instructions are carried through",
			payload:          `{"supportedVersions":[],"capabilities":{},"instructions":"line\u0000one\u2028caf\u00e9"}`,
			wantVersions:     []string{},
			wantInstructions: "line\x00one\u2028caf\u00e9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseDiscoverResult(json.RawMessage(tc.payload))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected the payload to be refused with %q, got %+v", tc.wantErr, result)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !slices.Equal(result.SupportedVersions, tc.wantVersions) {
				t.Fatalf("supported versions = %v, want %v", result.SupportedVersions, tc.wantVersions)
			}
			if result.Instructions != tc.wantInstructions {
				t.Fatalf("instructions = %q, want %q", result.Instructions, tc.wantInstructions)
			}
			if result.ServerInfo.Name != tc.wantServerName {
				t.Fatalf("server name = %q, want %q", result.ServerInfo.Name, tc.wantServerName)
			}
		})
	}
}

func TestParseInitializeResult(t *testing.T) {
	tests := []struct {
		name             string
		payload          string
		wantErr          string
		wantVersion      string
		wantInstructions string
	}{
		{
			name:        "a complete result",
			payload:     `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantVersion: "2025-11-25",
		},
		{
			name: "instructions are carried through",
			payload: `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"},` +
				`"instructions":"Be nice."}`,
			wantVersion:      "2025-11-25",
			wantInstructions: "Be nice.",
		},
		{
			// An optional member in another shape is read as if it were absent,
			// rather than costing the handshake.
			name: "instructions that are not a string are read as none",
			payload: `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"},` +
				`"instructions":42}`,
			wantVersion: "2025-11-25",
		},
		{
			name: "null instructions are read as none",
			payload: `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"},` +
				`"instructions":null}`,
			wantVersion: "2025-11-25",
		},
		{
			// The version is the one member that decides the wire shape, so a
			// non-string one settled on nothing this client can speak.
			name:    "a version that is not a string settled on none",
			payload: `{"protocolVersion":20251125,"capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "the server chose protocol version [none]; this client supports [2025-11-25, 2025-06-18]",
		},
		{
			name:    "the version is reported before a malformed capability member",
			payload: `{"protocolVersion":"2025-03-26","capabilities":42,"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "the server chose protocol version [2025-03-26]",
		},
		{
			name:        "the older supported version",
			payload:     `{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantVersion: "2025-06-18",
		},
		{
			name:    "a version this client does not speak through initialize",
			payload: `{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "the server chose protocol version [2025-03-26]; this client supports [2025-11-25, 2025-06-18]",
		},
		{
			name:    "the discovery version cannot be settled through initialize",
			payload: `{"protocolVersion":"2026-07-28","capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "the server chose protocol version [2026-07-28]",
		},
		{
			name:    "a missing version",
			payload: `{"capabilities":{},"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "the server chose protocol version [none]",
		},
		{
			name:    "a missing server identity",
			payload: `{"protocolVersion":"2025-11-25","capabilities":{}}`,
			wantErr: "invalid initialize response from server",
		},
		{
			name:    "a server identity without a version",
			payload: `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"S"}}`,
			wantErr: "invalid initialize response from server",
		},
		{
			name:    "missing capabilities",
			payload: `{"protocolVersion":"2025-11-25","serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "invalid initialize response from server",
		},
		{
			name:    "capabilities that are not an object",
			payload: `{"protocolVersion":"2025-11-25","capabilities":[],"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "invalid initialize response from server",
		},
		{
			name:    "null capabilities",
			payload: `{"protocolVersion":"2025-11-25","capabilities":null,"serverInfo":{"name":"S","version":"1.0.0"}}`,
			wantErr: "invalid initialize response from server",
		},
		{
			name:    "a result that is not an object",
			payload: `"2025-11-25"`,
			wantErr: "invalid initialize response from server",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseInitializeResult(json.RawMessage(tc.payload))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected the payload to be refused with %q, got %+v", tc.wantErr, result)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if result.ProtocolVersion != tc.wantVersion {
				t.Fatalf("protocol version = %q, want %q", result.ProtocolVersion, tc.wantVersion)
			}
			if result.Instructions != tc.wantInstructions {
				t.Fatalf("instructions = %q, want %q", result.Instructions, tc.wantInstructions)
			}
		})
	}
}

func TestPreferredProtocolVersion(t *testing.T) {
	tests := []struct {
		name     string
		offered  []string
		want     ProtocolVersion
		wantFind bool
	}{
		{name: "the newest mutual version wins", offered: []string{"2025-06-18", "2026-07-28", "2025-11-25"}, want: LatestProtocolVersion, wantFind: true},
		{name: "order on the wire does not decide", offered: []string{"2025-11-25", "2025-06-18"}, want: ProtocolV20251125, wantFind: true},
		{name: "a single legacy version", offered: []string{"2025-06-18"}, want: ProtocolV20250618, wantFind: true},
		{name: "no mutual version", offered: []string{"2027-01-01", "2024-11-05"}, wantFind: false},
		{name: "nothing offered", offered: nil, wantFind: false},
		{name: "an empty version is not a version", offered: []string{""}, wantFind: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := preferredProtocolVersion(tc.offered...)
			if ok != tc.wantFind {
				t.Fatalf("found = %v, want %v", ok, tc.wantFind)
			}
			if ok && got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

// FuzzParseDiscoverResult exercises the parser with server-controlled input: it
// must either refuse the payload or return a result whose versions are the
// strings the server sent, and never panic.
func FuzzParseDiscoverResult(f *testing.F) {
	for _, seed := range []string{
		`{"supportedVersions":["\u0000\ud83d\ude00"],"capabilities":{}}`,
		`{"supportedVersions":[],"capabilities":{},"instructions":"hi"}`,
		`{"supportedVersions":[1,"2025-11-25"],"capabilities":{"tools":{}}}`,
		`{"supportedVersions":null,"capabilities":null}`,
		`{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"S","version":"1"}},"supportedVersions":[],"capabilities":{}}`,
		`{"supportedVersions":["2026-07-28"],"capabilities":{}}`,
		`{`, `[]`, `null`, `0`, `""`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload string) {
		if !json.Valid([]byte(payload)) {
			return
		}
		result, err := parseDiscoverResult(json.RawMessage(payload))
		if err != nil {
			if result != nil {
				t.Fatalf("a refused payload returned a result: %+v", result)
			}
			return
		}
		if result.Capabilities == nil {
			t.Fatal("an accepted result must carry capabilities")
		}
		if result.SupportedVersions == nil {
			t.Fatal("an accepted result must carry a version list")
		}
		// Whatever survives the filter has to be a version the server actually
		// advertised, so a negotiated version can never be invented here.
		var raw struct {
			SupportedVersions []any `json:"supportedVersions"`
		}
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			t.Fatalf("payload decoded once but not twice: %v", err)
		}
		for _, version := range result.SupportedVersions {
			if !slices.ContainsFunc(raw.SupportedVersions, func(entry any) bool { return entry == any(version) }) {
				t.Fatalf("version %q is not in the payload %q", version, payload)
			}
		}
	})
}
