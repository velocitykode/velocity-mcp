package client

import (
	"context"
	"encoding/json"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file covers the mirrored tool parameters: the annotations a tool's
// inputSchema carries, the headers a call renders from them, and what the
// client does with a tool whose annotations the specification forbids.

// annotatedTool builds a tools/list entry annotating its "region" property with
// the given header name.
func annotatedTool(header string) map[string]any {
	return map[string]any{
		"name":        "execute_sql",
		"description": "Runs SQL",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string", mirrorAnnotation: header},
				"query":  map[string]any{"type": "string"},
			},
		},
	}
}

// refusedTool builds a tools/list entry whose annotation sits on a property
// under an array's items, which no property chain reaches: a definition this
// client refuses to advertise.
func refusedTool() map[string]any {
	return map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"list": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"region": map[string]any{"type": "string", mirrorAnnotation: "Region"},
						},
					},
				},
			},
		},
	}
}

// scriptedLifetimeMs is the lifetime a scripted catalogue states for itself, in
// the unit the wire member is in. The specification requires a server to state
// one on every listing, and this one is long enough that no test outlives it on
// the local clock: a test about what happens once it has run out states its own.
const scriptedLifetimeMs = 300000

// toolsFrame builds a tools/list result carrying the given entries, with the
// caching hints the specification requires a server to state on it.
func toolsFrame(tools ...map[string]any) string {
	return toolsFrameKeptFor(scriptedLifetimeMs, tools...)
}

// toolsFrameKeptFor is toolsFrame stating the given ttlMs member, which is
// written as given: a test of a hint no client can use passes one. A nil
// lifetime leaves the member out, as a server that predates the hints does.
func toolsFrameKeptFor(lifetime any, tools ...map[string]any) string {
	entries := make([]any, 0, len(tools))
	for _, tool := range tools {
		entries = append(entries, tool)
	}
	result := map[string]any{"resultType": "complete", "tools": entries, "cacheScope": "private"}
	if lifetime != nil {
		result["ttlMs"] = lifetime
	}
	return resultFrame(result)
}

// toolCallFrame builds a tools/call result carrying one text block.
func toolCallFrame(text string) string {
	return resultFrame(map[string]any{
		"resultType": "complete",
		"content":    []any{map[string]any{"type": "text", "text": text}},
		"isError":    false,
	})
}

// discoveryClient builds a client that has settled on the discovery revision
// over a scripted transport replaying the given frames after the handshake.
func discoveryClient(t *testing.T, frames ...string) (*Client, *scriptedTransport) {
	t.Helper()
	s := newScriptedTransport(append([]string{discoverFrame(LatestProtocolVersion)}, frames...)...)
	return newScriptedClient(s), s
}

// TestMirroredParametersTravelInHeaders asserts an annotated argument reaches
// the server in its own header, encoded by the same rules as the other mirrored
// values, and that the body still carries it.
func TestMirroredParametersTravelInHeaders(t *testing.T) {
	tests := []struct {
		name       string
		schemaType string
		value      any
		want       string
	}{
		{name: "a string travels verbatim", schemaType: "string", value: "us-west1", want: "us-west1"},
		{name: "a unicode string is wrapped", schemaType: "string", value: "café", want: "=?base64?Y2Fmw6k=?="},
		{name: "a header injection attempt is wrapped", schemaType: "string", value: "a\r\nX-Evil: 1", want: "=?base64?YQ0KWC1FdmlsOiAx?="},
		{name: "an integer is rendered in decimal", schemaType: "integer", value: float64(42), want: "42"},
		{name: "a negative integer keeps its sign", schemaType: "integer", value: -7, want: "-7"},
		{name: "the largest safe integer is carried", schemaType: "integer", value: float64(1<<53 - 1), want: "9007199254740991"},
		{name: "a true boolean is lowercase", schemaType: "boolean", value: true, want: "true"},
		{name: "a false boolean is lowercase", schemaType: "boolean", value: false, want: "false"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := annotatedTool("Region")
			properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			properties["region"] = map[string]any{"type": tc.schemaType, mirrorAnnotation: "Region"}

			c, s := discoveryClient(t, toolsFrame(tool), toolCallFrame("done"))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			if len(tools) != 1 {
				t.Fatalf("listed %d tools, want 1", len(tools))
			}

			result, err := tools[0].Call(context.Background(), map[string]any{"region": tc.value, "query": "select 1"})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.Text() != "done" {
				t.Fatalf("text = %q, want done", result.Text())
			}

			headers := s.headersAt(2)
			if got := headers["Mcp-Param-Region"]; got != tc.want {
				t.Fatalf("Mcp-Param-Region = %q, want %q", got, tc.want)
			}
			// The mirrored headers stand beside the standard ones, never
			// instead of them.
			if headers[methodHeader] != "tools/call" || headers[nameHeader] != "execute_sql" {
				t.Fatalf("standard headers = %v", headers)
			}
			// The body still states the argument: the header mirrors it, it
			// does not replace it.
			if got := s.frame(t, 2).Params["arguments"].(map[string]any)["region"]; got == nil {
				t.Fatalf("the call body dropped the mirrored argument: %v", s.frame(t, 2).Params)
			}
		})
	}
}

// TestMirroredParametersAreOmittedWhenAbsent asserts the header is sent only for
// an argument the call actually carries: the specification requires it to be
// omitted for a missing or null value, and a server told otherwise would reject
// the call.
func TestMirroredParametersAreOmittedWhenAbsent(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]any
	}{
		{name: "the argument is absent", arguments: map[string]any{"query": "select 1"}},
		{name: "the argument is null", arguments: map[string]any{"region": nil, "query": "select 1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t, toolsFrame(annotatedTool("Region")), toolCallFrame("done"))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			if _, err := tools[0].Call(context.Background(), tc.arguments); err != nil {
				t.Fatalf("call: %v", err)
			}
			if _, present := s.headersAt(2)["Mcp-Param-Region"]; present {
				t.Fatalf("a header was sent for an argument the call does not carry: %v", s.headersAt(2))
			}
		})
	}
}

// TestNestedMirroredParameterIsReachable asserts a property nested under
// another object's properties is mirrored from its exact path.
func TestNestedMirroredParameterIsReachable(t *testing.T) {
	tool := map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"region": map[string]any{"type": "string", mirrorAnnotation: "Region"},
					},
				},
			},
		},
	}
	c, s := discoveryClient(t, toolsFrame(tool), toolCallFrame("done"))
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	arguments := map[string]any{"target": map[string]any{"region": "us-west1"}}
	if _, err := tools[0].Call(context.Background(), arguments); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := s.headersAt(2)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// nestedAnnotatedTool builds a tools/list entry annotating a "region" property
// of the given JSON type, nested under a "target" object.
func nestedAnnotatedTool(schemaType string) map[string]any {
	return map[string]any{
		"name": "execute_sql",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"region": map[string]any{"type": schemaType, mirrorAnnotation: "Region"},
					},
				},
			},
		},
	}
}

// TestMirroredValuesAreReadFromTheEncodedArguments asserts the header is
// derived from the arguments as the body states them, not from the Go value the
// caller happened to build them with: an argument is anything that encodes to
// the JSON the schema describes, and every one of these calls sends a body the
// server accepts, so every one must send the header that goes with it.
func TestMirroredValuesAreReadFromTheEncodedArguments(t *testing.T) {
	type target struct {
		Region string `json:"region"`
	}
	type region string

	tests := []struct {
		name  string
		value any
	}{
		{name: "a decoded object", value: map[string]any{"region": "us-west1"}},
		{name: "a map of a concrete type", value: map[string]string{"region": "us-west1"}},
		{name: "a struct", value: target{Region: "us-west1"}},
		{name: "a pointer to a struct", value: &target{Region: "us-west1"}},
		{name: "raw JSON", value: json.RawMessage(`{"region":"us-west1"}`)},
		{name: "a named string type", value: map[string]any{"region": region("us-west1")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t, toolsFrame(nestedAnnotatedTool("string")), toolCallFrame("done"))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			if _, err := tools[0].Call(context.Background(), map[string]any{"target": tc.value}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := s.headersAt(2)["Mcp-Param-Region"]; got != "us-west1" {
				t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
			}
			// The header mirrors the body: what the server reads in the frame
			// is the value the header states.
			arguments, _ := s.frame(t, 2).Params["arguments"].(map[string]any)
			nested, _ := arguments["target"].(map[string]any)
			if nested["region"] != "us-west1" {
				t.Fatalf("the body states %v, which the header does not mirror", arguments)
			}
		})
	}
}

// countingRegion is an argument that chooses its own JSON encoding and states
// a different value every time it is asked for one. Nothing forbids a caller
// from passing such a value, and the server only ever sees one of them: the one
// the frame carries.
type countingRegion struct{ encodings *int }

func (r countingRegion) MarshalJSON() ([]byte, error) {
	*r.encodings++
	return json.Marshal("region-" + strconv.Itoa(*r.encodings))
}

// TestMirroredValuesComeFromTheFrameTheyTravelWith asserts the header mirrors
// the body of its own attempt rather than a second encoding of the arguments.
// An argument that encodes itself is asked for its JSON once, and the value the
// frame carries is the value the header states; deriving the header from a
// fresh encoding would state one the server never received and earn a header
// mismatch for input the caller got right.
func TestMirroredValuesComeFromTheFrameTheyTravelWith(t *testing.T) {
	c, s := discoveryClient(t, toolsFrame(annotatedTool("Region")), toolCallFrame("done"))
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}

	encodings := 0
	if _, err := tools[0].Call(context.Background(), map[string]any{
		"region": countingRegion{encodings: &encodings},
	}); err != nil {
		t.Fatalf("call: %v", err)
	}

	arguments, _ := s.frame(t, 2).Params["arguments"].(map[string]any)
	body, _ := arguments["region"].(string)
	if body != "region-1" {
		t.Fatalf("the frame states region = %v, want region-1", arguments["region"])
	}
	if got := s.headersAt(2)["Mcp-Param-Region"]; got != body {
		t.Fatalf("Mcp-Param-Region = %q, want %q: the header contradicts the body it travels with", got, body)
	}
	if encodings != 1 {
		t.Fatalf("the arguments were encoded %d times, want once: every encoding past the frame's own can state another value", encodings)
	}
}

// TestMirroredIntegersAreReadFromTheEncodedArguments asserts the same for the
// number form: the value is read as the body writes it, so precision beyond
// what a float64 holds is refused rather than rounded into a header that
// contradicts the frame.
func TestMirroredIntegersAreReadFromTheEncodedArguments(t *testing.T) {
	type shard int

	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "a decoded JSON number", value: float64(42), want: "42"},
		{name: "a Go integer", value: 42, want: "42"},
		{name: "a named integer type", value: shard(42), want: "42"},
		{name: "an unsigned integer", value: uint64(42), want: "42"},
		{name: "a raw JSON number", value: json.RawMessage(`42`), want: "42"},
		{name: "a raw JSON number in exponent form", value: json.RawMessage(`1e2`), want: "100"},
		{name: "a raw JSON number with a zero fraction", value: json.RawMessage(`42.0`), want: "42"},
		{name: "the largest safe integer", value: int64(1<<53 - 1), want: "9007199254740991"},
		// One past the safe range is refused rather than mirrored as the
		// rounded value a float64 would carry it as.
		{name: "one past the safe range", value: int64(1 << 53), want: ""},
		{name: "a fraction", value: json.RawMessage(`42.5`), want: ""},
		// A fraction too fine for a float64 to hold is still a fraction: the
		// body states it as written, so a header stating the whole number it
		// rounds to would contradict the frame it travels with.
		{name: "a fraction beyond double precision", value: json.RawMessage(`1.0000000000000001`), want: ""},
		{name: "a long fraction of an integer", value: json.RawMessage(`42.00000000000000000001`), want: ""},
		{name: "a fraction written with an exponent", value: json.RawMessage(`1.5e0`), want: ""},
		{name: "a value below one", value: json.RawMessage(`1e-1`), want: ""},
		{name: "a negative fraction", value: json.RawMessage(`-7.25`), want: ""},
		{name: "a whole number written with a negative exponent", value: json.RawMessage(`4200e-2`), want: "42"},
		{name: "a signed exponent", value: json.RawMessage(`4.2e+1`), want: "42"},
		{name: "a negative zero", value: json.RawMessage(`-0.0`), want: "0"},
		{name: "an exponent no integer could hold", value: json.RawMessage(`1e1000`), want: ""},
		// Zero is zero however far the exponent moves the point, including an
		// exponent written past the range a machine word holds: the body states
		// the whole number zero, and the header may state it too.
		{name: "a zero raised past the range of a machine word", value: json.RawMessage(`0e99999999999999999999`), want: "0"},
		{name: "a zero lowered past the range of a machine word", value: json.RawMessage(`-0.000e-99999999999999999999`), want: "0"},
		{name: "a digit raised past the range of a machine word", value: json.RawMessage(`1e99999999999999999999`), want: ""},
		{name: "a digit lowered past the range of a machine word", value: json.RawMessage(`1e-99999999999999999999`), want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A call the definition in hand cannot mirror sends the client back
			// for that definition, which this server restates unchanged.
			answer := toolCallFrame("done")
			if tc.want == "" {
				answer = toolsFrame(nestedAnnotatedTool("integer"))
			}
			c, s := discoveryClient(t, toolsFrame(nestedAnnotatedTool("integer")), answer)
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			arguments := map[string]any{"target": map[string]any{"region": tc.value}}
			_, err = tools[0].Call(context.Background(), arguments)

			if tc.want == "" {
				if err == nil {
					t.Fatal("expected the call to be refused")
				}
				if !strings.Contains(err.Error(), "cannot be mirrored into the [Mcp-Param-Region] header as an [integer]") {
					t.Fatalf("error = %q", err.Error())
				}
				if got := s.methods(); slices.Contains(got, "tools/call") {
					t.Fatalf("the refused call still reached the wire: %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := s.headersAt(2)["Mcp-Param-Region"]; got != tc.want {
				t.Fatalf("Mcp-Param-Region = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInvalidAnnotationsExcludeTheTool asserts a tool definition that breaks the
// annotation rules is left out of the catalogue, that the rest of the catalogue
// survives it, and that the reason is recoverable.
func TestInvalidAnnotationsExcludeTheTool(t *testing.T) {
	object := func(members map[string]any) map[string]any { return members }

	tests := []struct {
		name        string
		inputSchema map[string]any
		wantReason  string
	}{
		{
			name: "an empty header name",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string", mirrorAnnotation: ""},
			}}),
			wantReason: "not a valid header name token",
		},
		{
			name: "a header name carrying a space",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string", mirrorAnnotation: "My Header"},
			}}),
			wantReason: "not a valid header name token",
		},
		{
			name: "a header name carrying a newline",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string", mirrorAnnotation: "Bad\nName"},
			}}),
			wantReason: "not a valid header name token",
		},
		{
			name: "a header name that is not a string",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string", mirrorAnnotation: float64(12)},
			}}),
			wantReason: "not a valid header name token",
		},
		{
			name: "an annotation on a number",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "number", mirrorAnnotation: "A"},
			}}),
			wantReason: "must sit on a string, integer, or boolean",
		},
		{
			name: "an annotation on an array",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "array", mirrorAnnotation: "A"},
			}}),
			wantReason: "must sit on a string, integer, or boolean",
		},
		{
			name: "an annotation on a property with no declared type",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{mirrorAnnotation: "A"},
			}}),
			wantReason: "must sit on a string, integer, or boolean",
		},
		{
			name: "two annotations differing only in case",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string", mirrorAnnotation: "Region"},
				"b": map[string]any{"type": "string", mirrorAnnotation: "region"},
			}}),
			wantReason: "is used more than once",
		},
		{
			name: "an annotation under an array's items",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "array", "items": map[string]any{
					"type": "object", "properties": map[string]any{
						"region": map[string]any{"type": "string", mirrorAnnotation: "Region"},
					},
				}},
			}}),
			wantReason: "sits outside the statically reachable properties",
		},
		{
			name: "an annotation under a composition keyword",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": map[string]any{"oneOf": []any{
					map[string]any{"type": "string", mirrorAnnotation: "Region"},
				}},
			}}),
			wantReason: "sits outside the statically reachable properties",
		},
		{
			name:        "an annotation on the schema root",
			inputSchema: object(map[string]any{"type": "object", mirrorAnnotation: "Region"}),
			wantReason:  "sits outside the statically reachable properties",
		},
		{
			name: "an annotation under a property whose schema is not an object",
			inputSchema: object(map[string]any{"type": "object", "properties": map[string]any{
				"a": []any{map[string]any{"type": "string", mirrorAnnotation: "Region"}},
			}}),
			wantReason: "sits outside the statically reachable properties",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			broken := map[string]any{"name": "execute_sql", "inputSchema": tc.inputSchema}
			plain := map[string]any{"name": "plain", "inputSchema": map[string]any{"type": "object"}}

			c, _ := discoveryClient(t, toolsFrame(broken, plain))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}

			names := make([]string, 0, len(tools))
			for _, tool := range tools {
				names = append(names, tool.Name)
			}
			if !slices.Equal(names, []string{"plain"}) {
				t.Fatalf("listed tools = %v, want [plain]: the invalid definition was advertised", names)
			}

			excluded := c.ExcludedTools()
			if len(excluded) != 1 || excluded[0].Name != "execute_sql" {
				t.Fatalf("excluded tools = %+v, want execute_sql", excluded)
			}
			if !strings.Contains(excluded[0].Err.Error(), tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", excluded[0].Err.Error(), tc.wantReason)
			}
		})
	}
}

// TestValidAnnotationsLeaveTheCatalogueIntact asserts the rules refuse nothing
// they are not meant to: a schema with no annotations, and one whose annotation
// sits on a nested property, both list normally and record no exclusion.
func TestValidAnnotationsLeaveTheCatalogueIntact(t *testing.T) {
	plain := map[string]any{"name": "plain", "inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}}
	c, _ := discoveryClient(t, toolsFrame(plain, annotatedTool("Region")))
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("listed %d tools, want 2", len(tools))
	}
	if got := c.ExcludedTools(); len(got) != 0 {
		t.Fatalf("excluded = %+v, want none", got)
	}
}

// TestInstanceDataIsNotAnAnnotation asserts the annotation is looked for where
// a schema describes a schema, not where it describes a value. A default, a
// const, an enum entry, and an example all hold instance data: an object stored
// there is an argument's value, so a member of it spelled like the annotation
// names no header and must not cost the tool its place in the catalogue.
func TestInstanceDataIsNotAnAnnotation(t *testing.T) {
	// data is an ordinary application value that happens to carry a member
	// spelled like the annotation.
	data := map[string]any{mirrorAnnotation: "ordinary application data"}

	tests := []struct {
		name    string
		keyword string
		value   any
	}{
		{name: "a default value", keyword: "default", value: data},
		{name: "a const value", keyword: "const", value: data},
		{name: "an enum of values", keyword: "enum", value: []any{data}},
		{name: "an example value", keyword: "examples", value: []any{data}},
		{name: "a default nested in an example", keyword: "examples", value: []any{map[string]any{"a": data}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := map[string]any{
				"name": "execute_sql",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"config": map[string]any{"type": "object", tc.keyword: tc.value},
						"region": map[string]any{"type": "string", mirrorAnnotation: "Region"},
					},
				},
			}

			c, s := discoveryClient(t, toolsFrame(tool), toolCallFrame("done"))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			if len(tools) != 1 {
				t.Fatalf("listed %d tools, want 1; excluded = %+v", len(tools), c.ExcludedTools())
			}
			if _, err := tools[0].Call(context.Background(), map[string]any{"region": "us-west1"}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := s.headersAt(2)["Mcp-Param-Region"]; got != "us-west1" {
				t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
			}
		})
	}
}

// TestAPropertyNamedLikeAKeywordIsStillAProperty asserts the reader places a
// member by the position it sits in rather than by the way it is spelled. The
// names in a properties map belong to the schema's author, so a property called
// "default" is a property: an annotation on it is mirrored, and one buried
// under a keyword that leads off the reachable chain still costs the tool its
// place in the catalogue, whatever the properties on the way there are called.
func TestAPropertyNamedLikeAKeywordIsStillAProperty(t *testing.T) {
	type schemaCase struct {
		name        string
		inputSchema map[string]any
		// property is the argument the call states, for a definition the client
		// accepts.
		property string
		// wantHeader is the value the mirrored header must carry, or the empty
		// string when the definition must be refused instead.
		wantHeader string
	}

	var tests []schemaCase
	// Each of these names a keyword whose value is instance data rather than a
	// schema, and each is also a name a schema may give a property.
	for _, keyword := range []string{"const", "default", "enum", "examples"} {
		annotated := map[string]any{"type": "string", mirrorAnnotation: "Region"}
		tests = append(tests,
			schemaCase{
				name: "an annotation on a property named " + keyword,
				inputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{keyword: annotated},
				},
				property:   keyword,
				wantHeader: "us-west1",
			},
			schemaCase{
				name: "an annotation on a property named " + keyword + " under an array's items",
				inputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"list": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type":       "object",
								"properties": map[string]any{keyword: annotated},
							},
						},
					},
				},
			},
			schemaCase{
				name: "an annotation on a property named " + keyword + " under a composition keyword",
				inputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"choice": map[string]any{"oneOf": []any{
							map[string]any{
								"type":       "object",
								"properties": map[string]any{keyword: annotated},
							},
						}},
					},
				},
			},
		)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := map[string]any{"name": "execute_sql", "inputSchema": tc.inputSchema}
			c, s := discoveryClient(t, toolsFrame(tool, plainTool("plain")), toolCallFrame("done"))

			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}

			if tc.wantHeader == "" {
				names := make([]string, 0, len(tools))
				for _, tool := range tools {
					names = append(names, tool.Name)
				}
				if !slices.Equal(names, []string{"plain"}) {
					t.Fatalf("listed tools = %v, want [plain]: the invalid definition was advertised", names)
				}
				excluded := c.ExcludedTools()
				if len(excluded) != 1 || excluded[0].Name != "execute_sql" {
					t.Fatalf("excluded tools = %+v, want execute_sql", excluded)
				}
				if !strings.Contains(excluded[0].Err.Error(), "sits outside the statically reachable properties") {
					t.Fatalf("reason = %q", excluded[0].Err.Error())
				}
				return
			}

			if len(tools) != 2 {
				t.Fatalf("listed %d tools, want 2; excluded = %+v", len(tools), c.ExcludedTools())
			}
			if _, err := tools[0].Call(context.Background(), map[string]any{tc.property: tc.wantHeader}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := s.headersAt(2)["Mcp-Param-Region"]; got != tc.wantHeader {
				t.Fatalf("Mcp-Param-Region = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

// TestADependencyKeyedByAPropertyNameIsNotAnAnnotation asserts the reader
// places the members of a dependency map by position too. The keys of
// dependentRequired, and of the dependency map draft 7 spells "dependencies",
// are property names the schema's author chose, so a property spelled like the
// annotation is the property it is and costs the tool nothing. What such a
// member holds is still read: a dependency stating a subschema is off the
// reachable chain, so an annotation there makes the definition invalid, and so
// does one in a member of a shape the keyword does not take.
func TestADependencyKeyedByAPropertyNameIsNotAnAnnotation(t *testing.T) {
	annotated := map[string]any{"type": "string", mirrorAnnotation: "Region"}

	tests := []struct {
		name        string
		inputSchema map[string]any
		// wantHeader is the value the mirrored header must carry, or the empty
		// string when the definition must be refused instead.
		wantHeader string
	}{
		{
			name: "a dependentRequired entry keyed by a property named like the annotation",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					mirrorAnnotation: map[string]any{"type": "string"},
					"region":         annotated,
				},
				"dependentRequired": map[string]any{mirrorAnnotation: []any{"region"}},
			},
			wantHeader: "us-west1",
		},
		{
			name: "a dependency naming properties, keyed by a property named like the annotation",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					mirrorAnnotation: map[string]any{"type": "string"},
					"region":         annotated,
				},
				"dependencies": map[string]any{mirrorAnnotation: []any{"region"}},
			},
			wantHeader: "us-west1",
		},
		{
			name: "a dependency stating a subschema, keyed by a property named like the annotation",
			inputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"region": annotated},
				"dependencies": map[string]any{mirrorAnnotation: map[string]any{
					"type":       "object",
					"properties": map[string]any{"billing": map[string]any{"type": "string"}},
				}},
			},
			wantHeader: "us-west1",
		},
		{
			name: "a dependentRequired below a property no chain reaches",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"list": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type":              "object",
							"properties":        map[string]any{"a": map[string]any{"type": "string"}},
							"dependentRequired": map[string]any{mirrorAnnotation: []any{"a"}},
						},
					},
					"region": annotated,
				},
			},
			wantHeader: "us-west1",
		},
		{
			name: "an annotation inside a dependency's subschema",
			inputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"region": map[string]any{"type": "string"}},
				"dependencies": map[string]any{"billing": map[string]any{
					"type":       "object",
					"properties": map[string]any{"zone": annotated},
				}},
			},
		},
		{
			name: "an annotation in a dependentRequired member of the wrong shape",
			inputSchema: map[string]any{
				"type":              "object",
				"properties":        map[string]any{"region": map[string]any{"type": "string"}},
				"dependentRequired": map[string]any{"billing": map[string]any{mirrorAnnotation: "Region"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := map[string]any{"name": "execute_sql", "inputSchema": tc.inputSchema}
			c, s := discoveryClient(t, toolsFrame(tool, plainTool("plain")), toolCallFrame("done"))

			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}

			if tc.wantHeader == "" {
				names := make([]string, 0, len(tools))
				for _, tool := range tools {
					names = append(names, tool.Name)
				}
				if !slices.Equal(names, []string{"plain"}) {
					t.Fatalf("listed tools = %v, want [plain]: the invalid definition was advertised", names)
				}
				excluded := c.ExcludedTools()
				if len(excluded) != 1 || excluded[0].Name != "execute_sql" {
					t.Fatalf("excluded tools = %+v, want execute_sql", excluded)
				}
				if !strings.Contains(excluded[0].Err.Error(), "sits outside the statically reachable properties") {
					t.Fatalf("reason = %q", excluded[0].Err.Error())
				}
				return
			}

			if len(tools) != 2 {
				t.Fatalf("listed %d tools, want 2; excluded = %+v", len(tools), c.ExcludedTools())
			}
			if _, err := tools[0].Call(context.Background(), map[string]any{"region": tc.wantHeader}); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := s.headersAt(2)["Mcp-Param-Region"]; got != tc.wantHeader {
				t.Fatalf("Mcp-Param-Region = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

// TestEveryAnnotatedPropertyMirrorsItsOwnValue asserts each annotation is
// mirrored from the property it sits on. The reader walks with one path it
// grows and truncates, so a parameter that kept that path rather than a copy of
// it would end up naming whichever property the walk reached last, and the call
// would state one argument's value under another's header.
func TestEveryAnnotatedPropertyMirrorsItsOwnValue(t *testing.T) {
	annotated := func(header string) map[string]any {
		return map[string]any{"type": "string", mirrorAnnotation: header}
	}
	object := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties}
	}

	tests := []struct {
		name        string
		inputSchema map[string]any
		arguments   map[string]any
		wantHeaders map[string]string
	}{
		{
			name: "an annotated property followed by a plain sibling",
			inputSchema: object(map[string]any{
				"alpha": annotated("Alpha"),
				"beta":  map[string]any{"type": "string"},
			}),
			arguments:   map[string]any{"alpha": "first", "beta": "second"},
			wantHeaders: map[string]string{"Mcp-Param-Alpha": "first"},
		},
		{
			name: "annotated properties at two depths sharing a prefix",
			inputSchema: object(map[string]any{
				"outer": object(map[string]any{"inner": annotated("Inner")}),
				"tail":  annotated("Tail"),
			}),
			arguments: map[string]any{
				"outer": map[string]any{"inner": "deep"},
				"tail":  "shallow",
			},
			wantHeaders: map[string]string{"Mcp-Param-Inner": "deep", "Mcp-Param-Tail": "shallow"},
		},
		{
			name: "two annotated properties under the same parent",
			inputSchema: object(map[string]any{
				"outer": object(map[string]any{
					"first":  annotated("First"),
					"second": annotated("Second"),
				}),
			}),
			arguments: map[string]any{
				"outer": map[string]any{"first": "one", "second": "two"},
			},
			wantHeaders: map[string]string{"Mcp-Param-First": "one", "Mcp-Param-Second": "two"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := map[string]any{"name": "execute_sql", "inputSchema": tc.inputSchema}
			c, s := discoveryClient(t, toolsFrame(tool), toolCallFrame("done"))

			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			if len(tools) != 1 {
				t.Fatalf("listed %d tools, want 1; excluded = %+v", len(tools), c.ExcludedTools())
			}
			if _, err := tools[0].Call(context.Background(), tc.arguments); err != nil {
				t.Fatalf("call: %v", err)
			}

			headers := s.headersAt(2)
			for name, want := range tc.wantHeaders {
				if got := headers[name]; got != want {
					t.Fatalf("%s = %q, want %q; headers = %v", name, got, want, headers)
				}
			}
			for name := range headers {
				if strings.HasPrefix(name, mirrorPrefix) && tc.wantHeaders[name] == "" {
					t.Fatalf("the call carried the unexpected mirrored header %s: %v", name, headers)
				}
			}
		})
	}
}

// nestedSchema builds an inputSchema whose properties chain is depth levels
// deep, with the innermost property annotated when annotate is set.
func nestedSchema(depth int, annotate bool) map[string]any {
	leaf := map[string]any{"type": "string"}
	if annotate {
		leaf[mirrorAnnotation] = "Region"
	}
	node := leaf
	for range depth {
		node = map[string]any{"type": "object", "properties": map[string]any{"a": node}}
	}
	return node
}

// nestedThroughItems builds an inputSchema nesting depth arrays, which is a
// chain the reader follows without any property on it.
func nestedThroughItems(depth int) map[string]any {
	node := map[string]any{"type": "object"}
	for range depth {
		node = map[string]any{"type": "array", "items": node}
	}
	return map[string]any{"type": "object", "properties": map[string]any{"a": node}}
}

// nestedThroughComposition builds an inputSchema nesting depth composition
// keywords, whose subschemas sit in arrays rather than under a name.
func nestedThroughComposition(depth int) map[string]any {
	node := map[string]any{"type": "object"}
	for range depth {
		node = map[string]any{"oneOf": []any{node}}
	}
	return map[string]any{"type": "object", "properties": map[string]any{"a": node}}
}

// TestDeeplyNestedSchemasAreBounded asserts the annotation reader does not
// follow a remote definition as far as the server cares to nest it, whichever
// keyword the nesting runs through: the walk over the reachable properties and
// the one over everything else are both bounded. A schema deeper than the
// reader follows is refused, with the rest of the catalogue intact; one within
// reach is read as usual.
func TestDeeplyNestedSchemasAreBounded(t *testing.T) {
	// 1000 levels is far deeper than any argument a caller could assemble, and
	// is well inside what a server can state in a few tens of kilobytes.
	const depth = 1000

	tests := []struct {
		name        string
		inputSchema map[string]any
	}{
		{name: "a chain of properties", inputSchema: nestedSchema(depth, false)},
		{name: "a chain of array items", inputSchema: nestedThroughItems(depth)},
		{name: "a chain of composition keywords", inputSchema: nestedThroughComposition(depth)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deep := map[string]any{"name": "deep", "inputSchema": tc.inputSchema}
			shallow := map[string]any{"name": "shallow", "inputSchema": nestedSchema(8, true)}

			c, _ := discoveryClient(t, toolsFrame(deep, shallow))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}

			names := make([]string, 0, len(tools))
			for _, tool := range tools {
				names = append(names, tool.Name)
			}
			if !slices.Equal(names, []string{"shallow"}) {
				t.Fatalf("listed tools = %v, want [shallow]: the unbounded definition was advertised", names)
			}
			excluded := c.ExcludedTools()
			if len(excluded) != 1 || excluded[0].Name != "deep" {
				t.Fatalf("excluded tools = %+v, want deep", excluded)
			}
			if !strings.Contains(excluded[0].Err.Error(), "nested deeper than") {
				t.Fatalf("reason = %q", excluded[0].Err.Error())
			}
		})
	}
}

// TestMirroredArgumentOfTheWrongTypeIsRefused asserts a call whose argument does
// not match the type the schema declared fails before it is sent: mirroring it
// anyway would put a header on the wire the body contradicts. The definition is
// read again first, and restating it settles the refusal.
func TestMirroredArgumentOfTheWrongTypeIsRefused(t *testing.T) {
	tests := []struct {
		name       string
		schemaType string
		value      any
	}{
		{name: "a number where a string was declared", schemaType: "string", value: float64(1)},
		{name: "a string where an integer was declared", schemaType: "integer", value: "42"},
		{name: "a fraction where an integer was declared", schemaType: "integer", value: 1.5},
		{name: "an integer beyond the safe range", schemaType: "integer", value: float64(1 << 53)},
		{name: "a string where a boolean was declared", schemaType: "boolean", value: "true"},
		{name: "an object where a string was declared", schemaType: "string", value: map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := annotatedTool("Region")
			tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["region"] =
				map[string]any{"type": tc.schemaType, mirrorAnnotation: "Region"}

			c, s := discoveryClient(t, toolsFrame(tool), toolsFrame(tool))
			tools, err := c.Tools(context.Background())
			if err != nil {
				t.Fatalf("tools: %v", err)
			}
			_, err = tools[0].Call(context.Background(), map[string]any{"region": tc.value})
			if err == nil {
				t.Fatal("expected the call to be refused")
			}
			if !strings.Contains(err.Error(), "cannot be mirrored into the [Mcp-Param-Region] header") {
				t.Fatalf("error = %q", err.Error())
			}
			if got := s.methods(); slices.Contains(got, "tools/call") {
				t.Fatalf("the refused call still reached the wire: %v", got)
			}
			want := []string{"server/discover", "tools/list", "tools/list"}
			if got := s.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v", got, want)
			}
		})
	}
}

// plainTool builds a tools/list entry whose schema annotates nothing.
func plainTool(name string) map[string]any {
	return map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
}

// emptyToolsFrame answers the catalogue read a call by name makes: this server
// advertises no tool at all, so nothing is mirrored.
func emptyToolsFrame() string { return toolsFrame() }

// TestCallingByNameReadsTheDefinitionFirst asserts a call made by name alone
// carries the mirrored headers on its first attempt. The headers are what an
// intermediary routes and authorizes on, and a request that reaches one without
// them may be refused on HTTP's terms rather than the protocol's, so the client
// reads the definition instead of waiting to be told what it is missing.
func TestCallingByNameReadsTheDefinitionFirst(t *testing.T) {
	c, s := discoveryClient(t, toolsFrame(annotatedTool("Region")), toolCallFrame("done"))

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	headers := s.headersAt(2)
	if got := headers["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1: the first attempt went out unmirrored", got)
	}
	if headers[methodHeader] != "tools/call" || headers[nameHeader] != "execute_sql" {
		t.Fatalf("standard headers = %v", headers)
	}
}

// TestTheCatalogueIsReadOnceForCallsByName asserts the definitions a listing
// read are remembered: neither a second call nor a call the catalogue does not
// advertise reads it again.
func TestTheCatalogueIsReadOnceForCallsByName(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("first"),
		toolCallFrame("second"),
		toolCallFrame("third"),
	)

	for _, name := range []string{"execute_sql", "execute_sql", "unlisted"} {
		if _, err := c.CallTool(context.Background(), name, map[string]any{"region": "us-west1"}); err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
	}
	want := []string{"server/discover", "tools/list", "tools/call", "tools/call", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	for _, index := range []int{2, 3} {
		if got := s.headersAt(index)["Mcp-Param-Region"]; got != "us-west1" {
			t.Fatalf("frame %d Mcp-Param-Region = %q, want us-west1", index, got)
		}
	}
	if _, present := s.headersAt(4)["Mcp-Param-Region"]; present {
		t.Fatalf("a tool the catalogue does not advertise was called with %v", s.headersAt(4))
	}
}

// TestAListingPrimesCallsByName asserts a caller that has already listed the
// tools pays for no second listing: the definition Tools read is the one the
// call by name mirrors from.
func TestAListingPrimesCallsByName(t *testing.T) {
	c, s := discoveryClient(t, toolsFrame(annotatedTool("Region")), toolCallFrame("done"))

	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if _, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	want := []string{"server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if got := s.headersAt(2)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestAWholeListingReplacesWhatWasKnown asserts a call by name mirrors from the
// catalogue as it now stands. A whole listing states every tool the server
// advertises, so a tool it has dropped, and one whose refreshed definition this
// client refuses, leave nothing behind for a later call to mirror.
func TestAWholeListingReplacesWhatWasKnown(t *testing.T) {
	tests := []struct {
		name string
		// second is the listing that replaces the one the client read first.
		second string
	}{
		{name: "the server no longer advertises the tool", second: toolsFrame()},
		{name: "the refreshed definition is one the client refuses", second: toolsFrame(refusedTool())},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t, toolsFrame(annotatedTool("Region")), tc.second, toolCallFrame("done"))

			for range 2 {
				if _, err := c.Tools(context.Background()); err != nil {
					t.Fatalf("tools: %v", err)
				}
			}
			if _, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"}); err != nil {
				t.Fatalf("call: %v", err)
			}

			want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
			if got := s.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v", got, want)
			}
			if _, present := s.headersAt(3)["Mcp-Param-Region"]; present {
				t.Fatalf("the call mirrored a definition the catalogue no longer states: %v", s.headersAt(3))
			}
		})
	}
}

// TestACappedListingDropsWhatItRefused asserts a listing that reads only part
// of the catalogue settles the tools it did read. A capped listing states
// nothing about the tools it never reached, so what it did carry is merged into
// what was known; a tool it refused it did reach, and the definition this
// client last read of it is one it will not mirror from, so the call states
// what the caller stated rather than headers read from a definition the client
// has just turned down.
func TestACappedListingDropsWhatItRefused(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(annotatedTool("Region")),
		toolsFrame(refusedTool()),
		toolCallFrame("done"),
	)

	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}
	capped, err := c.Tools(context.Background(), 5)
	if err != nil {
		t.Fatalf("capped tools: %v", err)
	}
	if len(capped) != 0 {
		t.Fatalf("the capped listing advertised %d tool(s), want none", len(capped))
	}
	excluded := c.ExcludedTools()
	if len(excluded) != 1 || excluded[0].Name != "execute_sql" {
		t.Fatalf("excluded tools = %+v, want execute_sql", excluded)
	}

	if _, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if _, present := s.headersAt(3)["Mcp-Param-Region"]; present {
		t.Fatalf("the call mirrored a definition the client refused: %v", s.headersAt(3))
	}
}

// TestAChannelFailureReadingTheCatalogueFailsTheCall asserts a call by name is
// not sent unmirrored because the read of the definition it needs was cut off.
// A failure of the channel is not the server's answer: nothing is settled about
// what the tool asks to be mirrored, and sending the call anyway would put it
// in front of an intermediary without the headers it routes and authorizes on,
// which is the very thing the definition is read for.
func TestAChannelFailureReadingTheCatalogueFailsTheCall(t *testing.T) {
	c, s := discoveryClient(t,
		// The catalogue read is answered with a frame that is not JSON-RPC 2.0,
		// which takes the connection down.
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
		// What a silent recovery would spend: a fresh handshake, and the call
		// sent over it with nothing mirrored.
		discoverFrame(LatestProtocolVersion),
		toolCallFrame("done"),
	)

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err == nil {
		t.Fatal("expected the call to fail with the failure of the catalogue read")
	}
	if !strings.Contains(err.Error(), "invalid JSON-RPC response from server") {
		t.Fatalf("error = %q, want the failure of the channel", err.Error())
	}
	want := []string{"server/discover", "tools/list"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: the call went out although its definition could not be read", got, want)
	}
	if c.Connected() {
		t.Fatal("a broken channel must drop the connection")
	}
}

// TestACatalogueRefusedOnProtocolTermsStillSendsTheCall is the other half: a
// server that refuses the listing is answering rather than failing, so the
// connection stands and the call is made with what the caller stated.
func TestACatalogueRefusedOnProtocolTermsStillSendsTheCall(t *testing.T) {
	c, s := discoveryClient(t, methodNotFoundFrame(), toolCallFrame("done"))

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if _, present := s.headersAt(2)["Mcp-Param-Region"]; present {
		t.Fatalf("a header was mirrored from a definition the server refused to state: %v", s.headersAt(2))
	}
	if !c.Connected() {
		t.Fatal("a refusal on protocol terms must leave the connection up")
	}
}

// TestNoCatalogueIsReadOnTheInitializeEra asserts the extra read belongs to the
// revision that defines the mirrored headers: on a connection that carries no
// request headers there is nothing to mirror, so a call by name goes straight
// out.
func TestNoCatalogueIsReadOnTheInitializeEra(t *testing.T) {
	s := newScriptedTransport(initializeFrame(ProtocolV20251125), toolCallFrame("done"))
	c := newScriptedClient(s).WithProtocolVersion(ProtocolV20251125)

	if _, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestHeaderMismatchRelistsAndRetriesOnce asserts the recovery the
// specification asks for: a server refusing the call because the mirrored
// headers do not match makes the client re-read the tool and repeat the call
// once with the headers the refreshed definition asks for. The definition the
// call started from is the one a listing read before the server changed it.
func TestHeaderMismatchRelistsAndRetriesOnce(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(plainTool("execute_sql")),
		errorFrame(CodeHeaderMismatch, "Header mismatch: The [Mcp-Param-Region] header is required.", nil),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "tools/call", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if _, present := s.headersAt(2)["Mcp-Param-Region"]; present {
		t.Fatal("the stale definition mirrored a header it does not declare")
	}
	if got := s.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("retry Mcp-Param-Region = %q, want us-west1", got)
	}
}

// regionTool builds the annotated tool with the given declared type for its
// mirrored property, which is what a server that has changed its mind about the
// definition advertises.
func regionTool(schemaType string) map[string]any {
	tool := annotatedTool("Region")
	tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["region"] =
		map[string]any{"type": schemaType, mirrorAnnotation: "Region"}
	return tool
}

// TestANewConnectionForgetsTheDefinitionsReadOverTheOldOne asserts a handshake
// settles who is speaking: definitions read over a connection describe the
// server that stated them, and a server reached again may be another one. The
// catalogue of the connection before is dropped rather than carried across, so
// the first call over the new one is weighed against a definition read from the
// server now answering.
//
// The definition a server changes while nobody is connected changes what the
// call has to carry either way round. One that declares another type for a
// mirrored property refuses input the server now accepts, which the call would
// at least be told; one that begins mirroring a property it did not mirror
// before refuses nothing at all, and the call would reach the server without
// the header an intermediary in front of it routes on. Neither is corrected by
// anything the server says, because neither call ever reaches it as it is.
func TestANewConnectionForgetsTheDefinitionsReadOverTheOldOne(t *testing.T) {
	tests := []struct {
		name      string
		before    map[string]any
		after     map[string]any
		arguments map[string]any
		want      string
	}{
		{
			name:      "the server now mirrors a parameter it did not mirror before",
			before:    plainTool("execute_sql"),
			after:     annotatedTool("Region"),
			arguments: map[string]any{"region": "us-west1"},
			want:      "us-west1",
		},
		{
			name:      "the server now declares another type for the mirrored parameter",
			before:    regionTool("string"),
			after:     regionTool("integer"),
			arguments: map[string]any{"region": 42},
			want:      "42",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, s := discoveryClient(t,
				toolsFrame(tc.before),
				discoverFrame(LatestProtocolVersion),
				toolsFrame(tc.after),
				toolCallFrame("done"),
			)
			if _, err := c.Tools(context.Background()); err != nil {
				t.Fatalf("tools: %v", err)
			}
			c.Disconnect()

			result, err := c.CallTool(context.Background(), "execute_sql", tc.arguments)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.Text() != "done" {
				t.Fatalf("text = %q, want done", result.Text())
			}
			want := []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"}
			if got := s.methods(); !slices.Equal(got, want) {
				t.Fatalf("methods = %v, want %v: the catalogue of the old connection was carried into the new one", got, want)
			}
			if got := s.headersAt(4)["Mcp-Param-Region"]; got != tc.want {
				t.Fatalf("Mcp-Param-Region = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAConnectionReplacedMidCallReadsTheDefinitionAgain asserts the definition
// a call holds is weighed against the connection the call actually travels
// over, not the one it was held on. A session the server has forgotten is
// renegotiated under the request, and the attempt that follows travels over a
// connection the definition was never read over: a server that has begun
// mirroring a parameter since would be sent the call without the header it now
// asks for, and nothing would say so, because the server answers such a call
// rather than refusing it. The attempt is refused by the client instead, the
// catalogue read again over the new connection, and the call sent with what the
// server now asks for.
func TestAConnectionReplacedMidCallReadsTheDefinitionAgain(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(plainTool("execute_sql")),
		discoverFrame(LatestProtocolVersion),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}
	// The first attempt at the call finds the session gone, which renegotiates
	// the connection under it and repeats it.
	s.failOnce["tools/call"] = errSessionExpired

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: the definition of the replaced connection settled the call", got, want)
	}
	if got := s.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestHavingNothingToMirrorHoldsACallToItsConnection asserts a call with
// nothing to mirror is held to the connection that had nothing to give it, the
// way a call with a definition is held to the connection the definition was
// read over. That the catalogue does not advertise the tool, that the server
// refused the listing, and that the revision settled mirrors nothing are each
// what one connection stated. The session is forgotten under the call, and the
// server reached again speaks the discovery revision and advertises the tool
// mirroring its region: the repeat travels over a connection that answer says
// nothing about, so it is refused by the client, the catalogue read again, and
// the call sent with the header the server now asks for.
func TestHavingNothingToMirrorHoldsACallToItsConnection(t *testing.T) {
	reachedAgain := []string{
		discoverFrame(LatestProtocolVersion),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	}
	tests := []struct {
		name string
		// before is what the first connection answers with, handshake included.
		before      []string
		wantMethods []string
	}{
		{
			name:        "a catalogue that does not advertise the tool",
			before:      []string{discoverFrame(LatestProtocolVersion), toolsFrame(plainTool("summarize"))},
			wantMethods: []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"},
		},
		{
			name:        "a listing refused on protocol terms",
			before:      []string{discoverFrame(LatestProtocolVersion), methodNotFoundFrame()},
			wantMethods: []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"},
		},
		{
			name: "a revision that mirrors nothing into headers",
			// The server reached again no longer answers the handshake it was
			// remembered by, which is what sends the client back to the probe.
			before: []string{methodNotFoundFrame(), initializeFrame(ProtocolV20251125), methodNotFoundFrame()},
			wantMethods: []string{
				"server/discover", "initialize", "notifications/initialized",
				"initialize", "server/discover", "tools/list", "tools/call",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedTransport(append(append([]string{}, tc.before...), reachedAgain...)...)
			s.failOnce["tools/call"] = errSessionExpired
			c := newScriptedClient(s)

			result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
			if err != nil {
				t.Fatalf("call: %v (methods = %v)", err, s.methods())
			}
			if result.Text() != "done" {
				t.Fatalf("text = %q, want done (methods = %v)", result.Text(), s.methods())
			}
			if got := s.methods(); !slices.Equal(got, tc.wantMethods) {
				t.Fatalf("methods = %v, want %v: what the replaced connection did not state settled the call", got, tc.wantMethods)
			}
			if got := s.headersAt(len(tc.wantMethods) - 1)["Mcp-Param-Region"]; got != "us-west1" {
				t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
			}
		})
	}
}

// TestAToolListedOverAnOldConnectionIsReadAgain asserts a tool value carries
// the connection its listing was read over. Holding the value is holding a
// definition, and one the caller kept across a handshake states what a server
// that is no longer answering asked for: calling it reads the definition again
// rather than mirroring the old one, or nothing at all where the server now
// asks for a header.
func TestAToolListedOverAnOldConnectionIsReadAgain(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(plainTool("execute_sql")),
		discoverFrame(LatestProtocolVersion),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	c.Disconnect()

	result, err := tools[0].Call(context.Background(), map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: a definition of the old connection settled the call", got, want)
	}
	if got := s.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestAListingThatCrossesAHandshakeSettlesNothing asserts a catalogue is only
// ever one server's. A listing renegotiated between its pages read them from
// more than one connection, so the pages it carries are no server's catalogue:
// stamping them with the connection standing when the last page arrived would
// let a definition of the connection before settle the headers of a later call.
// The listing returns what it read, and the call that follows reads the
// catalogue again over the connection it travels on.
func TestAListingThatCrossesAHandshakeSettlesNothing(t *testing.T) {
	s := newScriptedTransport(
		discoverFrame(LatestProtocolVersion),
		resultFrame(map[string]any{
			"resultType": "complete",
			"tools":      []any{plainTool("execute_sql")},
			"nextCursor": "page-2",
		}),
		discoverFrame(LatestProtocolVersion),
		toolsFrame(plainTool("summarize")),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	// The session is gone by the time the second page is asked for, which
	// renegotiates the connection in the middle of the listing.
	s.beforeSend = func(method string) {
		if method == "tools/list" {
			s.beforeSend = nil
			s.sendErr = errSessionExpired
		}
	}
	c := newScriptedClient(s)

	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("listed %d tools, want the pages of both connections", len(tools))
	}

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{
		"server/discover", "tools/list", "server/discover", "tools/list", "tools/list", "tools/call",
	}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: a listing of two connections settled the call", got, want)
	}
	if got := s.headersAt(5)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestAListingIsStampedWithTheConnectionItTravelledOver asserts a listing
// belongs to the connection its page arrived over, however soon after it a
// handshake settles. A caller replacing the connection while the page is in
// flight is held at the exchange until the page is in, and settles its
// handshake the moment the exchange lets go: a listing that asked which
// connection stands only then would be told the new one, and its single page
// would pass for the catalogue of a server that never stated it. The tool the
// listing returned is of the first connection, so calling it reads the
// definition again and carries the header the server reached again asks for.
func TestAListingIsStampedWithTheConnectionItTravelledOver(t *testing.T) {
	s := newScriptedTransport(
		discoverFrame(LatestProtocolVersion),
		toolsFrame(plainTool("execute_sql")),
		discoverFrame(LatestProtocolVersion),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	c := newScriptedClient(s)

	// The second connection is settled in the very interval the stamp must not
	// be read in: after the exchange that carried the page has let go of the
	// gate, before the listing goes on with what that exchange returned. It is
	// entered on purpose, so nothing here depends on how goroutines are
	// scheduled.
	var listed, reconnected bool
	var reconnectErr error
	s.beforeSend = func(method string) { listed = listed || method == "tools/list" }
	c.proto.released = func() {
		if !listed || reconnected {
			return
		}
		reconnected = true
		c.Disconnect()
		reconnectErr = c.Connect(context.Background())
	}

	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if !reconnected {
		t.Fatal("no connection was settled after the page arrived: the test did not enter the interval it guards")
	}
	if reconnectErr != nil {
		t.Fatalf("reconnect: %v", reconnectErr)
	}
	if len(tools) != 1 {
		t.Fatalf("listed %d tools, want 1", len(tools))
	}
	if got := c.proto.connectionGeneration(); got != 2 {
		t.Fatalf("connection = %d, want the second one to be standing", got)
	}
	if tools[0].generation == 2 {
		t.Fatal("the listing was stamped with the connection settled after its page arrived")
	}

	result, err := tools[0].Call(context.Background(), map[string]any{"region": "us-west1"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: a definition of the first connection settled a call over the second", got, want)
	}
	if got := s.headersAt(4)["Mcp-Param-Region"]; got != "us-west1" {
		t.Fatalf("Mcp-Param-Region = %q, want us-west1", got)
	}
}

// TestAHeldDefinitionThatCannotMirrorTheCallIsReadAgain asserts a call refused
// before it is sent is not the end of it. A definition is only ever what the
// server last stated, and one it has changed while the connection stands
// refuses input the server would now accept, with no request reaching it to say
// so: the catalogue is read again and the call repeated with what the server
// now declares.
func TestAHeldDefinitionThatCannotMirrorTheCallIsReadAgain(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(regionTool("string")),
		toolsFrame(regionTool("integer")),
		toolCallFrame("done"),
	)
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": 42})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if got := s.headersAt(3)["Mcp-Param-Region"]; got != "42" {
		t.Fatalf("Mcp-Param-Region = %q, want 42", got)
	}
}

// TestAHeldDefinitionThatNoLongerMirrorsSendsTheCallPlain asserts the re-read
// settles what the call carries even when the refreshed definition mirrors
// nothing: a server that has dropped the annotation asks for no header, so the
// call goes out without one rather than being refused for a header it no longer
// has to state.
func TestAHeldDefinitionThatNoLongerMirrorsSendsTheCallPlain(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(regionTool("string")),
		toolsFrame(plainTool("execute_sql")),
		toolCallFrame("done"),
	)
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": 42})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
	want := []string{"server/discover", "tools/list", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if got, present := s.headersAt(3)["Mcp-Param-Region"]; present {
		t.Fatalf("Mcp-Param-Region = %q, want no header at all", got)
	}
}

// TestADefinitionReadForTheCallIsNotReadAgain asserts the re-read is spent only
// where a newer definition could exist. A call by name reads the definition
// itself, and what it read states the terms the server states now: a call that
// definition refuses is refused, without asking the server to state them twice.
func TestADefinitionReadForTheCallIsNotReadAgain(t *testing.T) {
	c, s := discoveryClient(t, toolsFrame(regionTool("integer")))

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err == nil {
		t.Fatal("expected the call to be refused")
	}
	if !strings.Contains(err.Error(), "cannot be mirrored into the [Mcp-Param-Region] header as an [integer]") {
		t.Fatalf("error = %q", err.Error())
	}
	want := []string{"server/discover", "tools/list"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestAChannelFailureRelistingAfterARefusalIsReported asserts the read a
// refused call sends the client back for is weighed the way every other is: a
// listing cut off by the channel settles nothing about what the tool asks to be
// mirrored, and the connection is down, so answering with the refusal would
// tell the caller its argument is wrong while what broke is the channel.
func TestAChannelFailureRelistingAfterARefusalIsReported(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(regionTool("string")),
		// The re-read is answered with a frame that is not JSON-RPC 2.0, which
		// takes the connection down.
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
	)
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": 42})
	if err == nil {
		t.Fatal("expected the call to fail with the failure of the re-read")
	}
	if strings.Contains(err.Error(), "cannot be mirrored") {
		t.Fatalf("error = %q, want the failure of the channel rather than the refusal it replaced", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid JSON-RPC response from server") {
		t.Fatalf("error = %q, want the failure of the channel", err.Error())
	}
	want := []string{"server/discover", "tools/list", "tools/list"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if c.Connected() {
		t.Fatal("a broken channel must drop the connection")
	}
}

// TestACatalogueIsReadOncePerConnection asserts the re-read belongs to the
// handshake and not to the call: while one connection stands, the definitions
// it stated are read once however many calls are made over it.
func TestACatalogueIsReadOncePerConnection(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(regionTool("string")),
		toolCallFrame("first"),
		toolCallFrame("second"),
	)
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("tools: %v", err)
	}
	for _, want := range []string{"first", "second"} {
		result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if result.Text() != want {
			t.Fatalf("text = %q, want %q", result.Text(), want)
		}
	}
	want := []string{"server/discover", "tools/list", "tools/call", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestHeaderMismatchIsNotRetriedWithoutNewHeaders asserts the retry is not a
// blind second attempt: when the refreshed definition mirrors nothing the first
// attempt did not, the server's refusal stands.
func TestHeaderMismatchIsNotRetriedWithoutNewHeaders(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(plainTool("execute_sql")),
		errorFrame(CodeHeaderMismatch, "Header mismatch: The [Mcp-Param-Region] header is required.", nil),
		toolsFrame(plainTool("execute_sql")),
	)

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err == nil {
		t.Fatal("expected the refusal to stand")
	}
	if !strings.Contains(err.Error(), "Header mismatch") {
		t.Fatalf("error = %q, want the server's refusal", err.Error())
	}
	want := []string{"server/discover", "tools/list", "tools/call", "tools/list"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestAChannelFailureRelistingAfterAMismatchIsReported asserts the read a
// header mismatch sends the client back for is weighed the way the read before
// the call is. A listing cut off by the channel is not the server's answer:
// nothing is settled about what the tool asks to be mirrored, and the
// connection is down. Answering with the mismatch would tell the caller its
// headers are wrong while what broke is the channel the retry would travel on.
func TestAChannelFailureRelistingAfterAMismatchIsReported(t *testing.T) {
	c, s := discoveryClient(t,
		toolsFrame(plainTool("execute_sql")),
		errorFrame(CodeHeaderMismatch, "Header mismatch: The [Mcp-Param-Region] header is required.", nil),
		// The re-read is answered with a frame that is not JSON-RPC 2.0, which
		// takes the connection down.
		`{"jsonrpc":"1.0","id":1,"result":{}}`,
	)

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err == nil {
		t.Fatal("expected the call to fail with the failure of the re-read")
	}
	if strings.Contains(err.Error(), "Header mismatch") {
		t.Fatalf("error = %q, want the failure of the channel rather than the refusal it replaced", err.Error())
	}
	if !strings.Contains(err.Error(), "invalid JSON-RPC response from server") {
		t.Fatalf("error = %q, want the failure of the channel", err.Error())
	}
	want := []string{"server/discover", "tools/list", "tools/call", "tools/list"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	if c.Connected() {
		t.Fatal("a broken channel must drop the connection")
	}
}

// TestOtherFailuresAreNotRetried asserts only a header mismatch triggers the
// re-listing: any other refusal is the answer to the call.
func TestOtherFailuresAreNotRetried(t *testing.T) {
	c, s := discoveryClient(t, emptyToolsFrame(), methodNotFoundFrame())

	_, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})
	if err == nil {
		t.Fatal("expected the call to fail")
	}
	want := []string{"server/discover", "tools/list", "tools/call"}
	if got := s.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
}

// TestNoMirroredHeadersOnTheInitializeEra asserts the mirrored headers belong to
// the revision that defines them: a connection settled through initialize sends
// no request headers at all, even for a tool whose schema annotates one.
func TestNoMirroredHeadersOnTheInitializeEra(t *testing.T) {
	s := newScriptedTransport(
		initializeFrame(ProtocolV20251125),
		toolsFrame(annotatedTool("Region")),
		toolCallFrame("done"),
	)
	c := newScriptedClient(s).WithProtocolVersion(ProtocolV20251125)

	tools, err := c.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if _, err := tools[0].Call(context.Background(), map[string]any{"region": "us-west1"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	for index := range s.methods() {
		if headers := s.headersAt(index); len(headers) != 0 {
			t.Fatalf("frame %d carried headers %v on the initialize era", index, headers)
		}
	}
}

// TestNoCatalogueIsReadWithoutAHeaderChannel asserts the extra read is spent
// only where its answer could be used. A transport with no header channel, such
// as stdio, carries no mirrored header whatever the definition says, so a call
// by name goes straight out rather than paying for a listing first.
func TestNoCatalogueIsReadWithoutAHeaderChannel(t *testing.T) {
	transport := newHeadlessTransport(discoverFrame(LatestProtocolVersion), toolCallFrame("done"))
	c := New(transport, testClientInfo())

	result, err := c.CallTool(context.Background(), "execute_sql", map[string]any{"region": "us-west1"})

	want := []string{"server/discover", "tools/call"}
	if got := transport.scripted.methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v, want %v: a listing was read for headers this transport cannot carry", got, want)
	}
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Text() != "done" {
		t.Fatalf("text = %q, want done", result.Text())
	}
}

// TestUnboundToolCannotBeCalled asserts a tool built by hand rather than listed
// through a client reports that plainly instead of sending anything.
func TestUnboundToolCannotBeCalled(t *testing.T) {
	_, err := Tool{Name: "orphan"}.Call(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "is not bound to a client") {
		t.Fatalf("error = %v", err)
	}
}

// FuzzParseMirroredParameters drives the annotation reader over arbitrary
// schemas. It runs on a definition an untrusted server supplied, so it must
// never panic, and whatever it accepts must render header names that cannot
// break the request framing.
func FuzzParseMirroredParameters(f *testing.F) {
	seeds := []string{
		`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`,
		`{"type":"object","properties":{"a":{"type":"integer","x-mcp-header":"A"},"b":{"type":"boolean","x-mcp-header":"B"}}}`,
		`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"string","x-mcp-header":"B"}}}}}`,
		`{"type":"object","properties":{"a":{"type":"array","items":{"x-mcp-header":"A"}}}}`,
		`{"x-mcp-header":"Root"}`,
		`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`,
		`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Bad Name"}}}`,
		`{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A\r\nX-Evil: 1"}}}`,
		`{"type":"object","properties":{"a":{"type":"string","default":{"x-mcp-header":"A"}}}}`,
		`{"type":"object","properties":{"a":{"type":"string","enum":[{"x-mcp-header":"A"}]}}}`,
		`{"type":"object","properties":{"a":{"type":"object","const":{"b":{"x-mcp-header":"A"}}}}}`,
		`{"type":"object","properties":[]}`,
		`{}`,
		`null`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			return
		}
		params, err := parseMirroredParameters(schema)
		if err != nil {
			if len(params) != 0 {
				t.Fatalf("a refused schema still yielded %d parameter(s)", len(params))
			}
			return
		}
		seen := map[string]struct{}{}
		for _, param := range params {
			if !isHeaderToken(param.name) {
				t.Fatalf("accepted the header name %q", param.name)
			}
			if strings.ContainsAny(param.header(), "\r\n") {
				t.Fatalf("accepted a header name that breaks the framing: %q", param.header())
			}
			switch param.kind {
			case "string", "integer", "boolean":
			default:
				t.Fatalf("accepted the declared type %q", param.kind)
			}
			key := strings.ToLower(param.name)
			if _, duplicate := seen[key]; duplicate {
				t.Fatalf("accepted the duplicate header name %q", param.name)
			}
			seen[key] = struct{}{}
		}
	})
}

// annotationStep is one step of the chain a fuzzed schema hides its annotation
// under: a property of an object, a keyword whose value is one or more
// subschemas, or a keyword whose value is instance data.
type annotationStep struct {
	name string
	// kind is 'p' for a property of a properties map, 'k' for a keyword
	// holding subschemas, and 'd' for a keyword holding instance data.
	kind byte
	// array holds the keyword's value in an array, and named holds it in a map
	// of names, which is the shape those keywords take.
	array bool
	named bool
}

// annotationSteps are the steps a fuzzed schema is built from. The property
// names deliberately include the keywords, which is the whole point: the same
// word means a property in one position and a keyword in another.
var annotationSteps = []annotationStep{
	{name: "region", kind: 'p'},
	{name: "const", kind: 'p'},
	{name: "default", kind: 'p'},
	{name: "enum", kind: 'p'},
	{name: "examples", kind: 'p'},
	{name: "items", kind: 'p'},
	{name: "properties", kind: 'p'},
	{name: "additionalProperties", kind: 'k'},
	{name: "if", kind: 'k'},
	{name: "items", kind: 'k'},
	{name: "not", kind: 'k'},
	{name: "propertyNames", kind: 'k'},
	{name: "allOf", kind: 'k', array: true},
	{name: "oneOf", kind: 'k', array: true},
	{name: "prefixItems", kind: 'k', array: true},
	{name: "$defs", kind: 'k', named: true},
	{name: "dependencies", kind: 'k', named: true},
	{name: "dependentSchemas", kind: 'k', named: true},
	{name: "patternProperties", kind: 'k', named: true},
	{name: "const", kind: 'd'},
	{name: "default", kind: 'd'},
	{name: "enum", kind: 'd', array: true},
	{name: "examples", kind: 'd', array: true},
}

// stepsFrom reads a chain of steps from fuzzed bytes, one step per byte.
func stepsFrom(raw []byte) []annotationStep {
	// Eight steps is well within the depth the reader follows and keeps a
	// generated schema small enough to read in a failure message.
	if len(raw) > 8 {
		raw = raw[:8]
	}
	steps := make([]annotationStep, 0, len(raw))
	for _, b := range raw {
		steps = append(steps, annotationSteps[int(b)%len(annotationSteps)])
	}
	return steps
}

// schemaHiding nests an annotated string property under the given chain.
func schemaHiding(steps []annotationStep) map[string]any {
	node := map[string]any{"type": "string", mirrorAnnotation: "Region"}
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		if step.kind == 'p' {
			node = map[string]any{"type": "object", "properties": map[string]any{step.name: node}}
			continue
		}
		var value any = node
		switch {
		case step.array:
			value = []any{node}
		case step.named:
			value = map[string]any{"member": node}
		}
		node = map[string]any{"type": "object", step.name: value}
	}
	return node
}

// FuzzAnnotationReachability holds the reader to the specification's rule for
// where an annotation may sit: it is honored when the schema root reaches it
// through properties alone, it is not an annotation at all when it sits in
// instance data, and anything else makes the definition invalid. The chain is
// the fuzzer's to choose, and what to expect of it is read from the chain
// rather than from the reader's own tables, so a member the reader misplaces
// shows up here whichever way it is spelled.
func FuzzAnnotationReachability(f *testing.F) {
	step := func(name string, kind byte) byte {
		for index, candidate := range annotationSteps {
			if candidate.name == name && candidate.kind == kind {
				return byte(index)
			}
		}
		f.Fatalf("no [%c] step named %q", kind, name)
		return 0
	}
	f.Add([]byte(nil))
	f.Add([]byte{step("region", 'p')})
	f.Add([]byte{step("region", 'p'), step("default", 'p')})
	f.Add([]byte{step("items", 'k'), step("default", 'p')})
	f.Add([]byte{step("oneOf", 'k'), step("const", 'p')})
	f.Add([]byte{step("region", 'p'), step("items", 'k'), step("examples", 'p')})
	f.Add([]byte{step("$defs", 'k'), step("enum", 'p')})
	f.Add([]byte{step("default", 'd')})
	f.Add([]byte{step("region", 'p'), step("enum", 'd')})
	f.Add([]byte{step("items", 'k'), step("examples", 'd'), step("region", 'p')})

	f.Fuzz(func(t *testing.T, raw []byte) {
		steps := stepsFrom(raw)

		// The annotation is reachable only when every step of the chain is a
		// property and there is at least one: the root's own annotation names
		// no property. It is not an annotation at all once the chain enters
		// instance data, whatever led there.
		reachable, data := len(steps) > 0, false
		for _, chosen := range steps {
			switch chosen.kind {
			case 'd':
				data = true
			case 'k':
				reachable = false
			}
			if data {
				break
			}
		}

		params, err := parseMirroredParameters(schemaHiding(steps))

		switch {
		case data:
			if err != nil {
				t.Fatalf("refused instance data as an annotation: %v", err)
			}
			if len(params) != 0 {
				t.Fatalf("read %d parameter(s) out of instance data", len(params))
			}
		case reachable:
			if err != nil {
				t.Fatalf("refused a reachable annotation: %v", err)
			}
			want := make([]string, 0, len(steps))
			for _, chosen := range steps {
				want = append(want, chosen.name)
			}
			if len(params) != 1 || !slices.Equal(params[0].path, want) {
				t.Fatalf("read %+v, want one parameter at %v", params, want)
			}
			if params[0].name != "Region" || params[0].kind != "string" {
				t.Fatalf("read the parameter %+v", params[0])
			}
		default:
			if err == nil {
				t.Fatalf("advertised an annotation no property chain reaches: read %+v", params)
			}
			if !strings.Contains(err.Error(), "sits outside the statically reachable properties") {
				t.Fatalf("reason = %q", err.Error())
			}
		}
	})
}

// exactNumberDigits bounds how many digits, and how far an exponent, the exact
// reader below follows. It stands far past the range a mirrored integer may
// carry and far past any number a caller writes, and only keeps a fuzzed number
// of absurd size from being read at the cost its size asks for.
const exactNumberDigits = 4096

// exactNumber reads a JSON number in exact rational arithmetic, reporting false
// for one written large enough that reading it exactly costs more than the
// check is worth. A number the bound turns away is either far outside the range
// a mirrored integer may carry or a fraction far below one, so nothing a header
// could state is left unchecked by it.
//
// A mantissa of nothing but zeros is read before the bound applies: it is the
// number zero whatever its exponent says, so a header may state it, and a bound
// that skipped it would hide a refusal of the whole number zero.
func exactNumber(text string) (*big.Rat, bool) {
	mantissa, exponent := text, "0"
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		mantissa, exponent = text[:index], text[index+1:]
	}
	digits, ok := new(big.Rat).SetString(mantissa)
	if !ok {
		return nil, false
	}
	if digits.Sign() == 0 {
		return new(big.Rat), true
	}
	if len(mantissa) > exactNumberDigits {
		return nil, false
	}
	power, err := strconv.Atoi(exponent)
	if err != nil || power > exactNumberDigits || power < -exactNumberDigits {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(text)
	return value, ok
}

// FuzzMirroredInteger drives the integer reader over arbitrary JSON numbers and
// holds it to both halves of the one property that matters: a value it accepts
// must be mirrored into a header stating exactly the number the body carries,
// and a whole number a header can carry must not be refused. The comparison is
// made in exact rational arithmetic rather than through the reader's own
// arithmetic, so a rounding the reader performs cannot hide here.
func FuzzMirroredInteger(f *testing.F) {
	seeds := []string{
		"0", "-0", "-0.0", "42", "42.0", "1e2", "1E+2", "4200e-2", "4.2e1",
		"1.0000000000000001", "42.00000000000000000001", "0.1", "-7.25",
		"9007199254740991", "9007199254740992", "-9007199254740991",
		"1e1000", "1e-1000", "123456789012345678901234567890",
		// Whole numbers written in forms a reader that stops at the digits it
		// expects would refuse.
		"100e-2", "-4200e-2", "10000000000000000000000000000e-20",
		"0.0000000000000000000000000000000001e40",
		// Exponents written past the range a machine word holds, on a mantissa
		// that is zero and on one that is not.
		"0e99999999999999999999", "-0.000e-99999999999999999999",
		"1e99999999999999999999", "1e-99999999999999999999",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return
		}
		number, isNumber := decoded.(json.Number)
		if !isNumber {
			return
		}

		limit := new(big.Rat).SetInt64(safeInteger)
		text, accepted := mirroredInteger(number)
		if !accepted {
			exact, ok := exactNumber(string(number))
			if ok && exact.IsInt() && exact.Cmp(limit) <= 0 && exact.Cmp(new(big.Rat).Neg(limit)) >= 0 {
				t.Fatalf("refused %q, which is the whole number %s", number, exact.RatString())
			}
			return
		}

		got, ok := new(big.Rat).SetString(text)
		if !ok {
			t.Fatalf("rendered %q for %q, which is not a number", text, number)
		}
		// The value the header states is compared against the body's own, read
		// exactly. A number written past the reader's bound is one it cannot
		// compare, and what the header states is held to the rest regardless.
		if want, readable := exactNumber(string(number)); readable && want.Cmp(got) != 0 {
			t.Fatalf("mirrored %q as %q, which states a different value", number, text)
		}
		if !got.IsInt() {
			t.Fatalf("mirrored %q as %q, which is not a whole number", number, text)
		}
		if got.Cmp(limit) > 0 || got.Cmp(new(big.Rat).Neg(limit)) < 0 {
			t.Fatalf("mirrored %q as %q, which is outside the safe range", number, text)
		}
	})
}
