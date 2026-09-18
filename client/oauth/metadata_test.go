package oauth

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAuthServerMetadataFromMap(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr bool
		check   func(t *testing.T, m *AuthServerMetadata)
	}{
		{
			name: "full document",
			doc: `{"issuer":"https://auth.example.com","authorization_endpoint":"https://auth.example.com/authorize",
				"token_endpoint":"https://auth.example.com/token","registration_endpoint":"https://auth.example.com/register",
				"code_challenge_methods_supported":["S256","plain"],"authorization_response_iss_parameter_supported":true,
				"token_endpoint_auth_methods_supported":["client_secret_basic"],"client_id_metadata_document_supported":true}`,
			check: func(t *testing.T, m *AuthServerMetadata) {
				if !m.ClientIDMetadataDocumentSupported {
					t.Fatal("client_id_metadata_document_supported must be parsed")
				}
				if !m.AuthorizationResponseIssParameterSupported {
					t.Fatal("authorization_response_iss_parameter_supported must be parsed")
				}
				if len(m.CodeChallengeMethodsSupported) != 2 {
					t.Fatalf("code challenge methods = %v", m.CodeChallengeMethodsSupported)
				}
			},
		},
		{
			name: "metadata document support absent defaults to false",
			doc:  `{"authorization_endpoint":"https://a/x","token_endpoint":"https://a/t"}`,
			check: func(t *testing.T, m *AuthServerMetadata) {
				if m.ClientIDMetadataDocumentSupported {
					t.Fatal("absent client_id_metadata_document_supported must not be treated as supported")
				}
			},
		},
		{
			name: "metadata document support of the wrong type is not support",
			doc:  `{"authorization_endpoint":"https://a/x","token_endpoint":"https://a/t","client_id_metadata_document_supported":"yes"}`,
			check: func(t *testing.T, m *AuthServerMetadata) {
				if m.ClientIDMetadataDocumentSupported {
					t.Fatal("a non-boolean value must not be treated as support")
				}
			},
		},
		{
			name:    "missing authorization endpoint",
			doc:     `{"token_endpoint":"https://a/t"}`,
			wantErr: true,
		},
		{
			name:    "missing token endpoint",
			doc:     `{"authorization_endpoint":"https://a/x"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal([]byte(tt.doc), &data); err != nil {
				t.Fatalf("fixture is not valid JSON: %v", err)
			}
			m, err := authServerMetadataFromMap(data)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error for an incomplete metadata document")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tt.check(t, m)
		})
	}
}

// FuzzAuthServerMetadataFromMap drives the authorization-server metadata parser,
// which consumes a document fetched from a remote server, with arbitrary JSON.
func FuzzAuthServerMetadataFromMap(f *testing.F) {
	for _, seed := range []string{
		`{"issuer":"https://a","authorization_endpoint":"https://a/x","token_endpoint":"https://a/t"}`,
		`{"authorization_endpoint":"https://a/x","token_endpoint":"https://a/t","code_challenge_methods_supported":["S256"]}`,
		`{"authorization_endpoint":1,"token_endpoint":{}}`,
		`{"code_challenge_methods_supported":"S256"}`,
		`{"code_challenge_methods_supported":[null,1,"S256"]}`,
		`{"client_id_metadata_document_supported":"true"}`,
		`[]`,
		`null`,
		`{}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil || data == nil {
			return
		}
		m, err := authServerMetadataFromMap(data)
		if err != nil {
			if m != nil {
				t.Fatalf("metadata returned alongside an error: %+v", m)
			}
			return
		}
		if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" {
			t.Fatalf("accepted a document without the required endpoints: %+v", m)
		}
		// Parsing the same document twice must yield the same view of the
		// server: a flow decides on PKCE and client identity from these fields.
		again, err := authServerMetadataFromMap(data)
		if err != nil || !reflect.DeepEqual(m, again) {
			t.Fatalf("parse is not deterministic for %q: %+v vs %+v (err %v)", raw, m, again, err)
		}
	})
}
