package client

import (
	"context"
	"reflect"
	"testing"
)

// This file covers the structured content of a tools/call result. A tool's
// outputSchema may describe any JSON value, so the decoder has to keep whatever
// the tool returned rather than insisting on an object.

// TestStructuredContentKeepsEveryJSONType asserts each shape a tool may return
// survives the decode, and that the reply is still read as a success.
func TestStructuredContentKeepsEveryJSONType(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{name: "an object", raw: `{"rows":2}`, want: map[string]any{"rows": float64(2)}},
		{name: "an array", raw: `[1,2]`, want: []any{float64(1), float64(2)}},
		{name: "an array of objects", raw: `[{"id":1}]`, want: []any{map[string]any{"id": float64(1)}}},
		{name: "a string", raw: `"ok"`, want: "ok"},
		{name: "a number", raw: `42`, want: float64(42)},
		{name: "a boolean", raw: `true`, want: true},
		{name: "an explicit null", raw: `null`, want: nil},
		{name: "an empty object", raw: `{}`, want: map[string]any{}},
		{name: "an empty array", raw: `[]`, want: []any{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":` + scriptRequestID +
				`,"result":{"resultType":"complete","content":[],"isError":false,"structuredContent":` + tc.raw + `}}`
			c, _ := discoveryClient(t, emptyToolsFrame(), frame)

			result, err := c.CallTool(context.Background(), "execute_sql", nil)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.IsError {
				t.Fatal("a completed result was read as a tool error")
			}
			if !result.HasStructuredContent {
				t.Fatal("the result carries structured content but reports none")
			}
			if !reflect.DeepEqual(result.StructuredContent, tc.want) {
				t.Fatalf("structuredContent = %#v, want %#v", result.StructuredContent, tc.want)
			}
		})
	}
}

// TestStructuredObjectNarrowsToAnObject asserts the convenience accessor reports
// the object case and declines the others, so a caller written for an object
// does not silently read one out of an array.
func TestStructuredObjectNarrowsToAnObject(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantOk bool
	}{
		{name: "an object", raw: `{"rows":2}`, wantOk: true},
		{name: "an array", raw: `[1,2]`, wantOk: false},
		{name: "a string", raw: `"ok"`, wantOk: false},
		{name: "an explicit null", raw: `null`, wantOk: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"jsonrpc":"2.0","id":` + scriptRequestID +
				`,"result":{"resultType":"complete","structuredContent":` + tc.raw + `}}`
			c, _ := discoveryClient(t, emptyToolsFrame(), frame)

			result, err := c.CallTool(context.Background(), "execute_sql", nil)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			object, ok := result.StructuredObject()
			if ok != tc.wantOk {
				t.Fatalf("StructuredObject ok = %v, want %v (got %#v)", ok, tc.wantOk, object)
			}
		})
	}
}

// TestAbsentStructuredContentIsDistinctFromNull asserts the two are told apart:
// a tool that returned none and a tool that returned an explicit null both
// decode to a nil value, and only the flag separates them.
func TestAbsentStructuredContentIsDistinctFromNull(t *testing.T) {
	tests := []struct {
		name    string
		result  string
		wantHas bool
	}{
		{name: "absent", result: `{"resultType":"complete","content":[]}`, wantHas: false},
		{name: "explicit null", result: `{"resultType":"complete","structuredContent":null}`, wantHas: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := discoveryClient(t, emptyToolsFrame(),
				`{"jsonrpc":"2.0","id":`+scriptRequestID+`,"result":`+tc.result+`}`)

			result, err := c.CallTool(context.Background(), "execute_sql", nil)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.HasStructuredContent != tc.wantHas {
				t.Fatalf("HasStructuredContent = %v, want %v", result.HasStructuredContent, tc.wantHas)
			}
			if result.StructuredContent != nil {
				t.Fatalf("structuredContent = %#v, want nil", result.StructuredContent)
			}
		})
	}
}
