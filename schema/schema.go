package schema

import "encoding/json"

// jsonType is the JSON Schema "type" keyword value for a property.
type jsonType string

const (
	typeString  jsonType = "string"
	typeInteger jsonType = "integer"
	typeNumber  jsonType = "number"
	typeBoolean jsonType = "boolean"
	typeArray   jsonType = "array"
	typeObject  jsonType = "object"
)

// Object is a fluent JSON Schema builder for an object schema, the shape used
// to describe MCP tool input and output arguments. It marshals to a valid
// JSON Schema object of the form:
//
//	{"type":"object","properties":{...},"required":[...]}
//
// Properties are emitted in the order they were declared so the generated
// schema is deterministic. The zero value is not usable; construct one with
// NewObject.
type Object struct {
	// order preserves declaration order for deterministic marshalling.
	order []string
	// props maps a property name to its builder.
	props map[string]*Property
	// additionalProperties, when set, emits the JSON Schema
	// "additionalProperties" keyword. nil omits it.
	additionalProperties *bool
}

// NewObject returns an empty object schema builder.
func NewObject() *Object {
	return &Object{
		props: make(map[string]*Property),
	}
}

// add registers (or replaces) a property of the given type and returns its
// builder for fluent configuration.
func (o *Object) add(name string, t jsonType) *Property {
	p := &Property{typ: t}
	if _, exists := o.props[name]; !exists {
		o.order = append(o.order, name)
	}
	o.props[name] = p
	return p
}

// String declares a string property and returns its builder.
func (o *Object) String(name string) *Property { return o.add(name, typeString) }

// Integer declares an integer property and returns its builder.
func (o *Object) Integer(name string) *Property { return o.add(name, typeInteger) }

// Number declares a number (float) property and returns its builder.
func (o *Object) Number(name string) *Property { return o.add(name, typeNumber) }

// Boolean declares a boolean property and returns its builder.
func (o *Object) Boolean(name string) *Property { return o.add(name, typeBoolean) }

// Array declares an array property and returns its builder. Use Items to
// describe the element schema.
func (o *Object) Array(name string) *Property { return o.add(name, typeArray) }

// Object declares a nested object property and returns its builder. The nested
// schema is built via the returned Property's Properties method.
func (o *Object) Object(name string) *Property { return o.add(name, typeObject) }

// Enum declares a string property constrained to the given set of allowed
// values and returns its builder. With no values supplied it behaves like a
// plain string property.
func (o *Object) Enum(name string, values ...string) *Property {
	p := o.add(name, typeString)
	if len(values) > 0 {
		p.enum = append([]string(nil), values...)
	}
	return p
}

// AdditionalProperties sets the JSON Schema "additionalProperties" keyword.
// Passing false (the common case for strict tool schemas) forbids properties
// not declared on this object. It returns the receiver for chaining.
func (o *Object) AdditionalProperties(allowed bool) *Object {
	o.additionalProperties = &allowed
	return o
}

// ToMap renders the object schema as an ordered-key-independent map suitable
// for embedding in a larger structure. Property order is preserved by the
// MarshalJSON path; ToMap is provided for callers that need the raw value.
func (o *Object) ToMap() map[string]any {
	props := make(map[string]any, len(o.order))
	var required []string
	for _, name := range o.order {
		p := o.props[name]
		props[name] = p.toMap()
		if p.required {
			required = append(required, name)
		}
	}

	m := map[string]any{
		"type":       string(typeObject),
		"properties": props,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	if o.additionalProperties != nil {
		m["additionalProperties"] = *o.additionalProperties
	}
	return m
}

// MarshalJSON implements json.Marshaler, emitting a valid JSON Schema object
// with properties in declaration order and a "required" array listing the
// properties marked required.
func (o *Object) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, 128)
	buf = append(buf, '{')

	buf = appendJSONKey(buf, "type")
	buf = append(buf, '"', 'o', 'b', 'j', 'e', 'c', 't', '"')

	buf = append(buf, ',')
	buf = appendJSONKey(buf, "properties")
	buf = append(buf, '{')
	for i, name := range o.order {
		if i > 0 {
			buf = append(buf, ',')
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		buf = append(buf, key...)
		buf = append(buf, ':')
		pb, err := json.Marshal(o.props[name].toMap())
		if err != nil {
			return nil, err
		}
		buf = append(buf, pb...)
	}
	buf = append(buf, '}')

	var required []string
	for _, name := range o.order {
		if o.props[name].required {
			required = append(required, name)
		}
	}
	if len(required) > 0 {
		rb, err := json.Marshal(required)
		if err != nil {
			return nil, err
		}
		buf = append(buf, ',')
		buf = appendJSONKey(buf, "required")
		buf = append(buf, rb...)
	}

	if o.additionalProperties != nil {
		buf = append(buf, ',')
		buf = appendJSONKey(buf, "additionalProperties")
		if *o.additionalProperties {
			buf = append(buf, 't', 'r', 'u', 'e')
		} else {
			buf = append(buf, 'f', 'a', 'l', 's', 'e')
		}
	}

	buf = append(buf, '}')
	return buf, nil
}

// appendJSONKey appends a quoted JSON object key followed by a colon. The key
// values used here are static schema keywords, so no escaping is required, but
// we still quote them explicitly for clarity.
func appendJSONKey(buf []byte, key string) []byte {
	buf = append(buf, '"')
	buf = append(buf, key...)
	buf = append(buf, '"', ':')
	return buf
}

// Property is a fluent builder for a single JSON Schema property. Obtain one
// from an Object's typed declaration methods (String, Integer, ...). All
// configuration methods return the receiver so calls can be chained.
type Property struct {
	typ         jsonType
	description string
	required    bool

	hasDefault bool
	def        any

	hasMin bool
	min    float64
	hasMax bool
	max    float64

	enum   []string
	items  *Property
	nested *Object
}

// Description sets the property's "description" keyword.
func (p *Property) Description(desc string) *Property {
	p.description = desc
	return p
}

// Required marks the property as required on its parent object. It does not
// emit a keyword on the property itself; the parent object collects required
// names into its "required" array.
func (p *Property) Required() *Property {
	p.required = true
	return p
}

// Default sets the property's "default" keyword to the given value.
func (p *Property) Default(v any) *Property {
	p.hasDefault = true
	p.def = v
	return p
}

// Min sets the lower bound for the property. For numeric properties it emits
// "minimum"; for strings "minLength"; for arrays "minItems".
func (p *Property) Min(v float64) *Property {
	p.hasMin = true
	p.min = v
	return p
}

// Max sets the upper bound for the property. For numeric properties it emits
// "maximum"; for strings "maxLength"; for arrays "maxItems".
func (p *Property) Max(v float64) *Property {
	p.hasMax = true
	p.max = v
	return p
}

// Enum constrains the property to the given set of allowed values, emitting the
// JSON Schema "enum" keyword.
func (p *Property) Enum(values ...string) *Property {
	p.enum = append([]string(nil), values...)
	return p
}

// Items describes the element schema of an array property and returns the item
// builder so it can be configured. Calling Items on a non-array property still
// records the item schema; callers should only use it on Array properties.
func (p *Property) Items(t string) *Property {
	item := &Property{typ: jsonType(t)}
	p.items = item
	return item
}

// Properties returns the nested object builder for an object property, creating
// it on first use, so nested fields can be declared.
func (p *Property) Properties() *Object {
	if p.nested == nil {
		p.nested = NewObject()
	}
	return p.nested
}

// toMap renders the property as a JSON Schema fragment. The keyword chosen for
// Min/Max depends on the property type so the output is valid JSON Schema.
func (p *Property) toMap() map[string]any {
	m := map[string]any{
		"type": string(p.typ),
	}
	if p.description != "" {
		m["description"] = p.description
	}
	if p.hasDefault {
		m["default"] = p.def
	}
	if len(p.enum) > 0 {
		m["enum"] = p.enum
	}

	minKey, maxKey := p.boundKeys()
	if p.hasMin {
		m[minKey] = numberOrInt(p.min, p.typ)
	}
	if p.hasMax {
		m[maxKey] = numberOrInt(p.max, p.typ)
	}

	if p.typ == typeArray && p.items != nil {
		m["items"] = p.items.toMap()
	}

	if p.typ == typeObject && p.nested != nil {
		nested := p.nested.ToMap()
		// Splice the nested object's keywords in directly so the property is
		// itself a complete object schema, rather than nesting under a key.
		if props, ok := nested["properties"]; ok {
			m["properties"] = props
		}
		if req, ok := nested["required"]; ok {
			m["required"] = req
		}
		if ap, ok := nested["additionalProperties"]; ok {
			m["additionalProperties"] = ap
		}
	}

	return m
}

// boundKeys returns the JSON Schema keyword names for Min and Max appropriate
// to the property's type.
func (p *Property) boundKeys() (minKey, maxKey string) {
	switch p.typ {
	case typeString:
		return "minLength", "maxLength"
	case typeArray:
		return "minItems", "maxItems"
	default:
		return "minimum", "maximum"
	}
}

// numberOrInt renders a bound as an int when the property is integer-typed and
// the value is whole, so the generated schema uses integer literals where
// appropriate.
func numberOrInt(v float64, t jsonType) any {
	switch t {
	case typeInteger, typeString, typeArray:
		if v == float64(int64(v)) {
			return int64(v)
		}
	}
	return v
}
