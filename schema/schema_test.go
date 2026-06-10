package schema_test

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/velocitykode/velocity-mcp/schema"
)

// marshalToMap marshals v and unmarshals it back into a generic map so tests
// can assert on structure independent of key ordering (except where we assert
// ordering directly on the raw bytes).
func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestObject_EmptyMarshalsToObjectWithEmptyProperties(t *testing.T) {
	s := schema.NewObject()
	m := marshalToMap(t, s)

	if m["type"] != "object" {
		t.Fatalf("type = %v, want object", m["type"])
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %T", m["properties"])
	}
	if len(props) != 0 {
		t.Fatalf("properties = %v, want empty", props)
	}
	if _, ok := m["required"]; ok {
		t.Fatalf("required should be omitted when no required props")
	}
}

func TestObject_StringPropertyWithDescriptionAndRequired(t *testing.T) {
	// Mirrors laravel's say-hi-tool golden schema.
	s := schema.NewObject()
	s.String("name").Description("The name of the person to greet").Required()

	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The name of the person to greet",
			},
		},
		"required": []any{"name"},
	}
	got := marshalToMap(t, s)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestObject_TypedProperties(t *testing.T) {
	tests := []struct {
		name     string
		build    func(o *schema.Object)
		propName string
		wantType string
	}{
		{"string", func(o *schema.Object) { o.String("a") }, "a", "string"},
		{"integer", func(o *schema.Object) { o.Integer("a") }, "a", "integer"},
		{"number", func(o *schema.Object) { o.Number("a") }, "a", "number"},
		{"boolean", func(o *schema.Object) { o.Boolean("a") }, "a", "boolean"},
		{"array", func(o *schema.Object) { o.Array("a") }, "a", "array"},
		{"object", func(o *schema.Object) { o.Object("a") }, "a", "object"},
		{"enum-is-string", func(o *schema.Object) { o.Enum("a", "x") }, "a", "string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := schema.NewObject()
			tc.build(s)
			m := marshalToMap(t, s)
			props := m["properties"].(map[string]any)
			prop := props[tc.propName].(map[string]any)
			if prop["type"] != tc.wantType {
				t.Fatalf("type = %v, want %v", prop["type"], tc.wantType)
			}
		})
	}
}

func TestObject_EnumValues(t *testing.T) {
	s := schema.NewObject()
	s.Enum("unit", "celsius", "fahrenheit")
	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["unit"].(map[string]any)
	enum, ok := prop["enum"].([]any)
	if !ok {
		t.Fatalf("enum missing: %#v", prop)
	}
	if !reflect.DeepEqual(enum, []any{"celsius", "fahrenheit"}) {
		t.Fatalf("enum = %v", enum)
	}
}

func TestObject_EnumWithNoValuesIsPlainString(t *testing.T) {
	s := schema.NewObject()
	s.Enum("u")
	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["u"].(map[string]any)
	if _, ok := prop["enum"]; ok {
		t.Fatalf("enum should be omitted with no values")
	}
	if prop["type"] != "string" {
		t.Fatalf("type = %v, want string", prop["type"])
	}
}

func TestProperty_EnumMethod(t *testing.T) {
	s := schema.NewObject()
	s.String("u").Enum("a", "b")
	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["u"].(map[string]any)
	if !reflect.DeepEqual(prop["enum"], []any{"a", "b"}) {
		t.Fatalf("enum = %v", prop["enum"])
	}
}

func TestProperty_Default(t *testing.T) {
	s := schema.NewObject()
	s.Integer("days").Default(7)
	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["days"].(map[string]any)
	if prop["default"] != float64(7) {
		t.Fatalf("default = %v (%T), want 7", prop["default"], prop["default"])
	}
}

func TestProperty_MinMaxKeywordsByType(t *testing.T) {
	tests := []struct {
		name    string
		build   func(o *schema.Object)
		prop    string
		minKey  string
		maxKey  string
		wantMin any
		wantMax any
	}{
		{
			name:   "integer-minimum-maximum",
			build:  func(o *schema.Object) { o.Integer("days").Min(1).Max(14) },
			prop:   "days",
			minKey: "minimum", maxKey: "maximum",
			wantMin: float64(1), wantMax: float64(14),
		},
		{
			name:   "number-minimum-maximum",
			build:  func(o *schema.Object) { o.Number("x").Min(0.5).Max(9.5) },
			prop:   "x",
			minKey: "minimum", maxKey: "maximum",
			wantMin: 0.5, wantMax: 9.5,
		},
		{
			name:   "string-length",
			build:  func(o *schema.Object) { o.String("name").Min(2).Max(40) },
			prop:   "name",
			minKey: "minLength", maxKey: "maxLength",
			wantMin: float64(2), wantMax: float64(40),
		},
		{
			name:   "array-items-bounds",
			build:  func(o *schema.Object) { o.Array("tags").Min(1).Max(5) },
			prop:   "tags",
			minKey: "minItems", maxKey: "maxItems",
			wantMin: float64(1), wantMax: float64(5),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := schema.NewObject()
			tc.build(s)
			m := marshalToMap(t, s)
			prop := m["properties"].(map[string]any)[tc.prop].(map[string]any)
			if prop[tc.minKey] != tc.wantMin {
				t.Fatalf("%s = %v, want %v", tc.minKey, prop[tc.minKey], tc.wantMin)
			}
			if prop[tc.maxKey] != tc.wantMax {
				t.Fatalf("%s = %v, want %v", tc.maxKey, prop[tc.maxKey], tc.wantMax)
			}
		})
	}
}

func TestProperty_ArrayItems(t *testing.T) {
	s := schema.NewObject()
	s.Array("tags").Items("string")
	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["tags"].(map[string]any)
	items, ok := prop["items"].(map[string]any)
	if !ok {
		t.Fatalf("items missing: %#v", prop)
	}
	if items["type"] != "string" {
		t.Fatalf("items.type = %v", items["type"])
	}
}

func TestProperty_ArrayItemsCanBeConfigured(t *testing.T) {
	s := schema.NewObject()
	s.Array("nums").Items("integer").Description("an int").Min(0)
	m := marshalToMap(t, s)
	items := m["properties"].(map[string]any)["nums"].(map[string]any)["items"].(map[string]any)
	if items["description"] != "an int" {
		t.Fatalf("items.description = %v", items["description"])
	}
	if items["minimum"] != float64(0) {
		t.Fatalf("items.minimum = %v", items["minimum"])
	}
}

func TestProperty_NestedObject(t *testing.T) {
	s := schema.NewObject()
	addr := s.Object("address").Properties()
	addr.String("city").Required()
	addr.String("zip")

	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["address"].(map[string]any)
	if prop["type"] != "object" {
		t.Fatalf("address.type = %v", prop["type"])
	}
	nestedProps, ok := prop["properties"].(map[string]any)
	if !ok {
		t.Fatalf("nested properties missing: %#v", prop)
	}
	if _, ok := nestedProps["city"]; !ok {
		t.Fatalf("nested city missing")
	}
	req, ok := prop["required"].([]any)
	if !ok || !reflect.DeepEqual(req, []any{"city"}) {
		t.Fatalf("nested required = %v", prop["required"])
	}
}

func TestProperty_PropertiesIsIdempotent(t *testing.T) {
	s := schema.NewObject()
	p := s.Object("a")
	first := p.Properties()
	second := p.Properties()
	if first != second {
		t.Fatalf("Properties returned different builders")
	}
}

func TestObject_AdditionalPropertiesFalse(t *testing.T) {
	s := schema.NewObject()
	s.String("a")
	s.AdditionalProperties(false)
	m := marshalToMap(t, s)
	v, ok := m["additionalProperties"]
	if !ok {
		t.Fatalf("additionalProperties missing")
	}
	if v != false {
		t.Fatalf("additionalProperties = %v, want false", v)
	}
}

func TestObject_AdditionalPropertiesTrue(t *testing.T) {
	s := schema.NewObject()
	s.AdditionalProperties(true)
	m := marshalToMap(t, s)
	if m["additionalProperties"] != true {
		t.Fatalf("additionalProperties = %v, want true", m["additionalProperties"])
	}
}

func TestObject_AdditionalPropertiesOmittedByDefault(t *testing.T) {
	s := schema.NewObject()
	m := marshalToMap(t, s)
	if _, ok := m["additionalProperties"]; ok {
		t.Fatalf("additionalProperties should be omitted by default")
	}
}

func TestObject_PropertyOrderPreserved(t *testing.T) {
	s := schema.NewObject()
	s.String("zebra")
	s.Integer("apple")
	s.Boolean("mango")

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"type":"object","properties":{"zebra":{"type":"string"},"apple":{"type":"integer"},"mango":{"type":"boolean"}}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestObject_RedeclaringPropertyReplacesKeepsOrder(t *testing.T) {
	s := schema.NewObject()
	s.String("a").Description("first")
	s.Integer("b")
	// Redeclare "a" as integer; it should keep its original position.
	s.Integer("a").Description("second")

	m := marshalToMap(t, s)
	props := m["properties"].(map[string]any)
	a := props["a"].(map[string]any)
	if a["type"] != "integer" || a["description"] != "second" {
		t.Fatalf("a = %#v, want integer/second", a)
	}

	// Order must be preserved: "a" stays first even after redeclaration. The
	// property-name ordering is the part MarshalJSON guarantees, so assert it
	// against the raw bytes by checking "a" precedes "b".
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	idxA := indexOf(string(b), `"a":`)
	idxB := indexOf(string(b), `"b":`)
	if idxA == -1 || idxB == -1 || idxA > idxB {
		t.Fatalf("property order not preserved: %s", b)
	}
}

// indexOf is a tiny helper returning the byte index of sub in s, or -1.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestObject_MultipleRequiredInOrder(t *testing.T) {
	s := schema.NewObject()
	s.String("a").Required()
	s.String("b")
	s.String("c").Required()
	m := marshalToMap(t, s)
	req := m["required"].([]any)
	if !reflect.DeepEqual(req, []any{"a", "c"}) {
		t.Fatalf("required = %v, want [a c]", req)
	}
}

func TestObject_ToMap(t *testing.T) {
	s := schema.NewObject()
	s.String("a").Required()
	s.AdditionalProperties(false)
	m := s.ToMap()
	if m["type"] != "object" {
		t.Fatalf("type = %v", m["type"])
	}
	if _, ok := m["properties"].(map[string]any); !ok {
		t.Fatalf("properties wrong type")
	}
	req, ok := m["required"].([]string)
	if !ok || !reflect.DeepEqual(req, []string{"a"}) {
		t.Fatalf("required = %v", m["required"])
	}
	if m["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %v", m["additionalProperties"])
	}
}

func TestObject_FullDesignExample(t *testing.T) {
	// The exact example from DESIGN.md.
	s := schema.NewObject()
	s.String("location").Description("City name").Required()
	s.Integer("days").Min(1).Max(14).Default(7)
	s.Enum("unit", "celsius", "fahrenheit")

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip to assert it is a valid JSON Schema object.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not valid json: %v", err)
	}
	if m["type"] != "object" {
		t.Fatalf("type = %v", m["type"])
	}
	props := m["properties"].(map[string]any)
	loc := props["location"].(map[string]any)
	if loc["type"] != "string" || loc["description"] != "City name" {
		t.Fatalf("location = %#v", loc)
	}
	days := props["days"].(map[string]any)
	if days["minimum"] != float64(1) || days["maximum"] != float64(14) || days["default"] != float64(7) {
		t.Fatalf("days = %#v", days)
	}
	unit := props["unit"].(map[string]any)
	if !reflect.DeepEqual(unit["enum"], []any{"celsius", "fahrenheit"}) {
		t.Fatalf("unit.enum = %v", unit["enum"])
	}
	req := m["required"].([]any)
	if !reflect.DeepEqual(req, []any{"location"}) {
		t.Fatalf("required = %v", req)
	}
}

func TestProperty_DefaultBoolAndString(t *testing.T) {
	tests := []struct {
		name  string
		build func(o *schema.Object)
		prop  string
		want  any
	}{
		{"bool", func(o *schema.Object) { o.Boolean("ok").Default(true) }, "ok", true},
		{"string", func(o *schema.Object) { o.String("s").Default("hi") }, "s", "hi"},
		{"float", func(o *schema.Object) { o.Number("n").Default(1.5) }, "n", 1.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := schema.NewObject()
			tc.build(s)
			m := marshalToMap(t, s)
			prop := m["properties"].(map[string]any)[tc.prop].(map[string]any)
			if prop["default"] != tc.want {
				t.Fatalf("default = %v, want %v", prop["default"], tc.want)
			}
		})
	}
}

func TestNumberOrInt_NonWholeNumberStaysFloat(t *testing.T) {
	s := schema.NewObject()
	s.String("name").Max(40.5) // non-whole on a string bound
	m := marshalToMap(t, s)
	prop := m["properties"].(map[string]any)["name"].(map[string]any)
	if prop["maxLength"] != 40.5 {
		t.Fatalf("maxLength = %v, want 40.5", prop["maxLength"])
	}
}

func TestObject_ConcurrentMarshal(t *testing.T) {
	// Build once, then marshal concurrently. Marshalling must not mutate the
	// builder, so this is race-free.
	s := schema.NewObject()
	s.String("a").Required()
	s.Integer("b").Min(0).Max(10)
	s.Enum("c", "x", "y")
	s.AdditionalProperties(false)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := json.Marshal(s); err != nil {
				t.Errorf("marshal: %v", err)
			}
		}()
	}
	wg.Wait()
}
