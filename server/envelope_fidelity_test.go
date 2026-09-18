package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velocitykode/velocity-mcp/content"
	"github.com/velocitykode/velocity-mcp/jsonrpc"
	"github.com/velocitykode/velocity-mcp/server"
)

// rawResultMethod answers with a result whose exact bytes the test pins. It
// bypasses any helper that would re-encode the payload, so what reaches the
// wire is what the handler wrote.
type rawResultMethod struct{ raw string }

func (m rawResultMethod) Handle(c *server.Context, req *jsonrpc.Request) (*jsonrpc.Response, error) {
	return jsonrpc.NewResult(req.ID, json.RawMessage(m.raw))
}

// TestResultEnvelopePreservesPayloadBytes asserts the envelope adds its two
// members without rewriting anything else in the result. Values a JSON number
// cannot survive as a float64 (an id past 2^53, an unsigned 64-bit maximum, a
// float carrying more digits than a float64 prints) must arrive digit for
// digit, because a host that reads a record key off a result and writes it back
// must address the same record.
func TestResultEnvelopePreservesPayloadBytes(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		expect []string
	}{
		{
			name:   "integer beyond float64 precision",
			raw:    `{"id":9007199254740993}`,
			expect: []string{`"id":9007199254740993`},
		},
		{
			name:   "unsigned 64-bit maximum",
			raw:    `{"u":18446744073709551615}`,
			expect: []string{`"u":18446744073709551615`},
		},
		{
			name:   "high precision decimal",
			raw:    `{"ratio":0.1234567890123456789}`,
			expect: []string{`"ratio":0.1234567890123456789`},
		},
		{
			name:   "exponent notation is not normalised",
			raw:    `{"big":1e400}`,
			expect: []string{`"big":1e400`},
		},
		{
			name:   "nested payload is untouched",
			raw:    `{"rows":[{"key":9223372036854775807},{"key":-9223372036854775808}]}`,
			expect: []string{`"key":9223372036854775807`, `"key":-9223372036854775808`},
		},
		{
			name:   "unicode literals and escaped control characters survive",
			raw:    `{"text":"é 中文 \u0007"}`,
			expect: []string{`"text":"é 中文 \u0007"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := server.New("demo", "1.0.0", server.WithMethod("custom/raw", rawResultMethod{raw: tt.raw}))
			res := handle(t, s, modernRequest(1, "custom/raw"))
			if res.Response.Error != nil {
				t.Fatalf("unexpected error: %+v", res.Response.Error)
			}
			got := string(res.Response.Result)
			for _, want := range tt.expect {
				if !strings.Contains(got, want) {
					t.Fatalf("result %s does not carry %s verbatim", got, want)
				}
			}
			// The envelope still did its job on the same payload.
			if !strings.Contains(got, `"resultType":"complete"`) {
				t.Fatalf("result %s carries no result type", got)
			}
			if !strings.Contains(got, server.MetaKeyServerInfo) {
				t.Fatalf("result %s carries no server info", got)
			}
		})
	}
}

// TestToolStructuredContentKeepsLargeIntegers asserts the fidelity holds on the
// path an application actually uses: a tool returning structured content with
// 64-bit keys, driven through tools/call.
func TestToolStructuredContentKeepsLargeIntegers(t *testing.T) {
	tool := server.NewTool("record", "returns a record with 64-bit keys").
		HandleFunc(func(ctx context.Context, req *server.Request) (*server.Response, error) {
			return server.NewResponse(content.NewText("ok")).WithStructuredContent(map[string]any{
				"id": int64(9007199254740993),
				"u":  uint64(18446744073709551615),
			}), nil
		})
	s := server.New("demo", "1.0.0", server.WithTools(tool))

	got := string(handle(t, s, modernRequest(1, "tools/call", `"name":"record"`)).Response.Result)
	for _, want := range []string{`"id":9007199254740993`, `"u":18446744073709551615`} {
		if !strings.Contains(got, want) {
			t.Fatalf("tool result %s does not carry %s verbatim", got, want)
		}
	}
}

// TestResultEnvelopeReplacesNonObjectMeta asserts a result whose _meta is not an
// object still ends up with a usable metadata bag rather than a member the
// envelope could not write into. The specification models _meta as an object,
// so the envelope owns the shape of the key it writes.
func TestResultEnvelopeReplacesNonObjectMeta(t *testing.T) {
	for _, raw := range []string{`{"_meta":"not-an-object"}`, `{"_meta":null}`, `{"_meta":[1,2]}`} {
		s := server.New("demo", "1.0.0", server.WithMethod("custom/raw", rawResultMethod{raw: raw}))
		result := decodeResult(t, handle(t, s, modernRequest(1, "custom/raw")).Response)
		info := serverInfoOf(t, result)
		if info["name"] != "demo" {
			t.Fatalf("server info = %v for _meta %s", info, raw)
		}
	}
}
