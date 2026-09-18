package client

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// This file implements the mirrored tool parameters of the streamable HTTP
// transport. A server may annotate a property of a tool's inputSchema with
// "x-mcp-header", and a client calling that tool must carry the argument's
// value in an "Mcp-Param-<name>" request header, so an intermediary can route
// or authorize the call without reading the body. The rules are the
// specification's: the annotation names an HTTP field-name token, is unique
// across the schema regardless of case, sits only on a string, integer, or
// boolean property, and is reachable from the schema root through "properties"
// alone. A tool that breaks any of them is one this client refuses to advertise.

const (
	// mirrorAnnotation is the inputSchema property naming the header a
	// parameter is mirrored into.
	mirrorAnnotation = "x-mcp-header"
	// mirrorPrefix is prepended to the annotation's value to form the header
	// name.
	mirrorPrefix = "Mcp-Param-"
	// safeInteger is the largest integer a mirrored value may carry. Values
	// beyond it cannot survive a round trip through the double-precision number
	// type the protocol's JSON is read with on many peers.
	safeInteger = int64(1)<<53 - 1
	// headerTokenChars is the set an HTTP field name is made of (the tchar
	// production of RFC 9110), which is what an annotation's value must be.
	headerTokenChars = "!#$%&'*+-.^_`|~0123456789" +
		"abcdefghijklmnopqrstuvwxyz" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	// maxSchemaDepth bounds how far into a tool's inputSchema the annotation
	// reader follows. A definition arrives from a remote server, so the walk
	// over it has to be bounded by something other than the server's goodwill;
	// no argument a caller could assemble is nested anywhere near this deep.
	maxSchemaDepth = 256
)

// instanceDataKeywords are the schema keywords whose value is instance data
// rather than a subschema. What sits under one of them describes a value the
// arguments may carry, so a member named like the annotation there is ordinary
// application data and names no header.
var instanceDataKeywords = map[string]struct{}{
	"const":    {},
	"default":  {},
	"enum":     {},
	"examples": {},
}

// subschemaKeywords are the schema keywords whose value is a subschema, or an
// array of them. What sits under one of them is a schema, and is read as one.
var subschemaKeywords = map[string]struct{}{
	"additionalItems":       {},
	"additionalProperties":  {},
	"allOf":                 {},
	"anyOf":                 {},
	"contains":              {},
	"contentSchema":         {},
	"else":                  {},
	"if":                    {},
	"items":                 {},
	"not":                   {},
	"oneOf":                 {},
	"prefixItems":           {},
	"propertyNames":         {},
	"then":                  {},
	"unevaluatedItems":      {},
	"unevaluatedProperties": {},
}

// subschemaMapKeywords are the schema keywords whose value maps a name of the
// keyword's own kind to a subschema. The names are the schema's, not the
// reader's: a member of one of these maps is a subschema whatever it is called,
// so a property spelled like a keyword is read as the property it is.
//
// The list form a dependency takes names properties rather than stating a
// schema, which the subschema reader passes over: an array of names carries
// nothing it could mistake for an annotation.
var subschemaMapKeywords = map[string]struct{}{
	"$defs":             {},
	"definitions":       {},
	"dependencies":      {},
	"dependentSchemas":  {},
	"patternProperties": {},
	"properties":        {},
}

// nameListKeywords are the schema keywords whose value maps a property name to
// a list of property names. Nothing under one of them is a schema, but its keys
// are the schema author's property names all the same, so a property called
// like the annotation must not be read as one.
var nameListKeywords = map[string]struct{}{
	"dependentRequired": {},
}

// isInstanceData reports whether a schema keyword holds instance data.
func isInstanceData(key string) bool {
	_, found := instanceDataKeywords[key]
	return found
}

// isSubschema reports whether a schema keyword holds a subschema or an array of
// them.
func isSubschema(key string) bool {
	_, found := subschemaKeywords[key]
	return found
}

// isSubschemaMap reports whether a schema keyword holds a map of names to
// subschemas.
func isSubschemaMap(key string) bool {
	_, found := subschemaMapKeywords[key]
	return found
}

// isNameList reports whether a schema keyword holds a map of names to lists of
// property names.
func isNameList(key string) bool {
	_, found := nameListKeywords[key]
	return found
}

// mirroredParameter is one annotated property: where its value sits in the
// argument bag, the header name it is mirrored into, and the JSON type the
// schema declares for it.
type mirroredParameter struct {
	path []string
	name string
	kind string
}

// header returns the request header this parameter is mirrored into.
func (p mirroredParameter) header() string { return mirrorPrefix + p.name }

// mirroredParameters is the set of annotated properties of one tool, in the
// order they are reached from the schema root.
type mirroredParameters []mirroredParameter

// parseMirroredParameters reads the annotated properties of a tool's
// inputSchema. It reports the first violation it finds, which is what makes the
// whole tool definition invalid.
func parseMirroredParameters(inputSchema map[string]any) (mirroredParameters, error) {
	params, err := mirroredIn(inputSchema, nil, false, 0)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(params))
	for _, param := range params {
		key := strings.ToLower(param.name)
		if _, duplicate := seen[key]; duplicate {
			return nil, newError("the [" + mirrorAnnotation + "] value [" + param.name + "] is used more than once")
		}
		seen[key] = struct{}{}
	}
	return params, nil
}

// mirroredIn collects the annotated properties of one schema node. consumed
// reports whether this node's own annotation has already been read, which is
// true for the schema of an annotated property and false for the root: an
// annotation anywhere a property chain does not reach is what makes a
// definition invalid, and the node's own annotation is the one exception.
//
// path is the property chain reaching this node, grown in place and truncated
// again on the way out: only a parameter that is actually annotated keeps a
// copy of its own, so a deeply nested schema costs the walk its depth rather
// than its depth squared.
//
// Keys are visited in sorted order so the violation a broken schema is reported
// with is the same one on every run.
func mirroredIn(schema map[string]any, path []string, consumed bool, depth int) (mirroredParameters, error) {
	if depth > maxSchemaDepth {
		return nil, tooDeep(path)
	}
	var out mirroredParameters
	for _, key := range sortedKeys(schema) {
		if key == mirrorAnnotation {
			if consumed {
				continue
			}
			return nil, unreachableAnnotation(path)
		}
		if isInstanceData(key) {
			continue
		}

		properties, isObject := schema[key].(map[string]any)
		if key != "properties" || !isObject {
			// Every other keyword (items, oneOf, $ref, if/then/else, ...) leads
			// away from the statically reachable chain, so an annotation under
			// one of them names a property no client could address.
			if err := keywordViolation(key, schema[key], path, depth+1); err != nil {
				return nil, err
			}
			continue
		}

		for _, property := range sortedKeys(properties) {
			path = append(path, property)
			member, isObject := properties[property].(map[string]any)
			if !isObject {
				// A property whose schema is not an object describes nothing
				// this walk can read; an annotation buried in it still names a
				// header no call could fill.
				if err := subschemaViolation(properties[property], path, depth+1); err != nil {
					return nil, err
				}
				path = path[:len(path)-1]
				continue
			}
			if _, annotated := member[mirrorAnnotation]; annotated {
				param, err := mirroredParameterOf(member, path)
				if err != nil {
					return nil, err
				}
				out = append(out, param)
			}
			nested, err := mirroredIn(member, path, true, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			path = path[:len(path)-1]
		}
	}
	return out, nil
}

// mirroredParameterOf reads the annotation on one property.
func mirroredParameterOf(member map[string]any, path []string) (mirroredParameter, error) {
	name, isString := member[mirrorAnnotation].(string)
	if !isString || !isHeaderToken(name) {
		return mirroredParameter{}, newError("the [" + mirrorAnnotation + "] value on [" +
			renderSchemaPath(path) + "] is not a valid header name token")
	}
	kind, _ := member["type"].(string)
	switch kind {
	case "string", "integer", "boolean":
	default:
		return mirroredParameter{}, newError("the [" + mirrorAnnotation + "] annotation on [" +
			renderSchemaPath(path) + "] must sit on a string, integer, or boolean")
	}
	return mirroredParameter{path: slices.Clone(path), name: name, kind: kind}, nil
}

// unreachableAnnotation reports an annotation the property chain cannot reach.
func unreachableAnnotation(path []string) error {
	where := renderSchemaPath(path)
	if where == "" {
		where = "the schema root"
	} else {
		where = "[" + where + "]"
	}
	return newError("an [" + mirrorAnnotation + "] annotation on " + where +
		" sits outside the statically reachable properties")
}

// tooDeep reports a schema nested deeper than the reader follows.
func tooDeep(path []string) error {
	where := renderSchemaPath(path)
	if where == "" {
		where = "the schema root"
	} else {
		where = "[" + where + "]"
	}
	return newError("the schema below " + where + " is nested deeper than [" +
		strconv.Itoa(maxSchemaDepth) + "] levels")
}

// keywordViolation reports what the value of one schema keyword carries that
// makes the definition invalid: an annotation at a position no property chain
// reaches, or a schema nested deeper than this reader follows. It returns nil
// when the value carries neither.
//
// The value is read by the position it sits in rather than by the way its
// members are spelled. Under a keyword holding instance data is a value the
// arguments may carry, so an object stored in a default or an example names no
// header however it is spelled; under a keyword holding subschemas is a schema,
// and the names of a properties map are the schema author's, so a property
// called "default" is a property and not a keyword. A keyword this reader does
// not know, or one whose value is not of the shape it takes, is read as plain
// JSON: an annotation anywhere in it is one no property chain reaches.
func keywordViolation(key string, value any, path []string, depth int) error {
	members, isObject := value.(map[string]any)
	switch {
	case isInstanceData(key):
		return nil
	case isSubschema(key):
		return subschemaViolation(value, path, depth)
	case isSubschemaMap(key) && isObject:
		return namedMembersViolation(members, path, depth, subschemaViolation)
	case isNameList(key) && isObject:
		return namedMembersViolation(members, path, depth, valueViolation)
	}
	return valueViolation(value, path, depth)
}

// namedMembersViolation reports what the members of a map keyed by names the
// schema author chose carry, each read by the reader its position takes. The
// names themselves are never read as keywords: a property named like the
// annotation is the property it is, wherever the schema names it.
func namedMembersViolation(members map[string]any, path []string, depth int, read func(any, []string, int) error) error {
	for _, name := range sortedKeys(members) {
		if err := read(members[name], path, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// subschemaViolation reports what a subschema position carries: one schema, or
// an array of them. A position holding anything else (the boolean form of a
// schema, or a member the keyword does not take) states no annotation.
func subschemaViolation(value any, path []string, depth int) error {
	if depth > maxSchemaDepth {
		return tooDeep(path)
	}
	switch node := value.(type) {
	case map[string]any:
		return schemaViolation(node, path, depth)
	case []any:
		for _, member := range node {
			if err := subschemaViolation(member, path, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// schemaViolation reports what one schema node below the reachable chain
// carries. Every annotation there is out of reach: the node itself is, so its
// own annotation is no exception.
func schemaViolation(schema map[string]any, path []string, depth int) error {
	if depth > maxSchemaDepth {
		return tooDeep(path)
	}
	for _, key := range sortedKeys(schema) {
		if key == mirrorAnnotation {
			return unreachableAnnotation(path)
		}
		if err := keywordViolation(key, schema[key], path, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// valueViolation reports an annotation anywhere in a fragment read as plain
// JSON, which is how a fragment whose meaning this reader cannot place is read:
// a schema is refused rather than advertised half understood.
func valueViolation(value any, path []string, depth int) error {
	if depth > maxSchemaDepth {
		return tooDeep(path)
	}
	switch node := value.(type) {
	case map[string]any:
		for _, key := range sortedKeys(node) {
			if key == mirrorAnnotation {
				return unreachableAnnotation(path)
			}
			if err := valueViolation(node[key], path, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, member := range node {
			if err := valueViolation(member, path, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// headers renders the request headers a call carries, read out of the encoded
// params of the very frame they travel with. A parameter the arguments do not
// carry (or carry as null) contributes no header, which is what tells the
// server not to expect one. A value that does not match the type the schema
// declared is refused rather than coerced: the header and the body would
// otherwise disagree and the server would reject the call anyway.
//
// The values are read from the body rather than from the Go values the caller
// assembled the bag with, and the body is read rather than encoded a second
// time: an argument is any value that encodes to the JSON the schema describes,
// so a nested struct, a map of a named type, or a raw JSON object all carry
// properties a mirrored path must reach, and a value that chooses its own
// encoding must not be asked for it twice or the header could state something
// the frame does not carry.
func (params mirroredParameters) headers(encodedParams json.RawMessage) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	values, err := encodedArguments(encodedParams)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(params))
	for _, param := range params {
		value, present := valueAtPath(values, param.path)
		if !present || value == nil {
			continue
		}
		text, ok := param.stringify(value)
		if !ok {
			return nil, newError("the [" + renderSchemaPath(param.path) + "] argument cannot be mirrored into the [" +
				param.header() + "] header as " + describeKind(param.kind))
		}
		out[param.header()] = encodeHeaderValue(text)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// encodedArguments reads the arguments member back out of the encoded params of
// a request, so every value below is the form the server is sent rather than
// whatever Go value produced it. Numbers are kept as written: an integer beyond
// the range a float64 holds exactly must be refused rather than rounded into a
// header the body contradicts.
func encodedArguments(encodedParams json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encodedParams))
	decoder.UseNumber()
	var members map[string]any
	if err := decoder.Decode(&members); err != nil {
		return nil, newError("the call arguments do not encode to a JSON object")
	}
	arguments, isObject := members["arguments"].(map[string]any)
	if !isObject {
		return nil, newError("the call arguments do not encode to a JSON object")
	}
	return arguments, nil
}

// stringify renders one argument value in the form its declared type is carried
// in, reporting false when the value is of another type.
func (p mirroredParameter) stringify(value any) (string, bool) {
	switch p.kind {
	case "string":
		text, ok := value.(string)
		return text, ok
	case "boolean":
		flag, ok := value.(bool)
		if !ok {
			return "", false
		}
		return strconv.FormatBool(flag), true
	default:
		return mirroredInteger(value)
	}
}

// mirroredInteger renders an integer argument. The value is the number as the
// body states it, which is accepted when it is a whole number within the range
// an integer can be carried in. It is read as an integer first, so a value
// beyond float64's exact range is refused rather than rounded; a number written
// in another form the specification's JSON allows (1.0 or 1e2) is read as the
// whole number it is.
func mirroredInteger(value any) (string, bool) {
	number, isNumber := value.(json.Number)
	if !isNumber {
		return "", false
	}
	if parsed, err := strconv.ParseInt(string(number), 10, 64); err == nil {
		return boundedInteger(parsed)
	}
	parsed, ok := integralNumber(string(number))
	if !ok {
		return "", false
	}
	return boundedInteger(parsed)
}

// boundedInteger renders a signed integer that fits the safe range.
func boundedInteger(value int64) (string, bool) {
	if value > safeInteger || value < -safeInteger {
		return "", false
	}
	return strconv.FormatInt(value, 10), true
}

// integralNumber reads a JSON number written with a fraction or an exponent,
// reporting the whole number it denotes and false for a fractional one.
//
// The digits are read as they are written rather than through a float64. The
// schema declared an integer, and a value such as 1.0000000000000001 rounds to
// 1 in binary floating point: mirroring the rounded value would state in the
// header a number the body does not carry, and would let a fractional argument
// pass for an integer.
func integralNumber(text string) (int64, bool) {
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}

	mantissa, power := text, ""
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		mantissa, power = text[:index], text[index+1:]
		if !isSignedDigits(power) {
			return 0, false
		}
	}

	whole, fraction := mantissa, ""
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		whole, fraction = mantissa[:index], mantissa[index+1:]
	}
	if !isDigits(whole) || (strings.Contains(mantissa, ".") && !isDigits(fraction)) {
		return 0, false
	}

	// The value is every digit read as one integer, with the decimal point
	// standing that many digits in; the exponent moves the point, nothing else.
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		// Every digit is a zero, so the number is zero however far the exponent
		// moves the point, and however large the exponent is written.
		return 0, true
	}

	exponent := 0
	if power != "" {
		parsed, err := strconv.Atoi(power)
		if err != nil {
			// An exponent beyond the range a machine word holds moves the point
			// further than any whole number a header may carry stands, in
			// either direction: what it states is out of range one way and a
			// fraction the other.
			return 0, false
		}
		exponent = parsed
	}

	// The point stands past the last significant digit by as much as the
	// exponent moves it, less the digits written behind the decimal point.
	point := len(digits) + exponent - len(fraction)
	if point < len(digits) {
		// Whatever sits past the point has to be nothing but zeros, or the
		// number is not the whole number the schema asked for.
		tail := max(point, 0)
		if strings.Trim(digits[tail:], "0") != "" {
			return 0, false
		}
		digits = digits[:tail]
	}
	// A value with more digits than an int64 holds is out of range whatever its
	// digits are, and is refused before it is written out.
	if point > 19 {
		return 0, false
	}
	parsed, err := strconv.ParseInt(digits+strings.Repeat("0", point-len(digits)), 10, 64)
	if err != nil {
		return 0, false
	}
	if negative {
		return -parsed, true
	}
	return parsed, true
}

// isSignedDigits reports whether a value is a run of decimal digits carrying an
// optional sign, which is what an exponent is written as.
func isSignedDigits(value string) bool {
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		value = value[1:]
	}
	return isDigits(value)
}

// isDigits reports whether a value is a non-empty run of decimal digits.
func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// describeKind names a declared type in a failure message with its article.
func describeKind(kind string) string {
	if kind == "integer" {
		return "an [integer]"
	}
	return "a [" + kind + "]"
}

// valueAtPath reads the value at an exact property path in an argument bag,
// reporting false when any step of the chain is missing or is not an object.
func valueAtPath(arguments map[string]any, path []string) (any, bool) {
	var current any = arguments
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// renderSchemaPath renders a property path for a failure message, in the JSON
// pointer form, so a nested property is named unambiguously even when a segment
// carries a slash of its own.
func renderSchemaPath(path []string) string {
	var out strings.Builder
	for _, segment := range path {
		out.WriteByte('/')
		out.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(segment))
	}
	return out.String()
}

// isHeaderToken reports whether a value is a non-empty HTTP field-name token,
// which excludes an empty name, whitespace, control characters, and anything
// outside US-ASCII.
func isHeaderToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if !strings.ContainsRune(headerTokenChars, rune(value[index])) {
			return false
		}
	}
	return true
}

// sortedKeys returns a decoded object's keys in a stable order.
func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sameHeaders reports whether two header sets carry the same names and values.
func sameHeaders(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if b[name] != value {
			return false
		}
	}
	return true
}
