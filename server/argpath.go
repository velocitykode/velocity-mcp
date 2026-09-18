package server

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// lookup resolves key as a path into the request arguments.
//
// Tool, resource, and prompt arguments are a JSON object whose properties are
// often nested, so a key addresses a value inside that structure: "user.name"
// walks into a nested object, "items.0" indexes an array element, and a "*"
// segment collects the rest of the path from every element of an array or
// object.
//
// A dot is always a separator and a "*" segment is always a wildcard, at every
// level of the walk. An empty segment is a member name like any other, because
// RFC 8259 section 4 allows the empty string as one: "user." reads the member
// "" of user, and "items.*." collects it from every element, where "items.*"
// collects the elements themselves.
//
// The velocity validation engine addresses a rule field more narrowly: it
// splits the field on dots and walks nested objects only, so it reaches neither
// an array element nor a wildcard. Request.Validate refuses a rule field the
// two would resolve differently (see ruleFieldFault), which is what makes the
// guarantee hold: the value a rule checked is the value the accessors then
// read.
//
// The MCP specification declares a tool's inputSchema as a JSON Schema object
// and puts no restriction on the property names it may carry, so an argument
// whose own name contains a dot, or is "*", is possible. So is a resource
// template variable named "user.id", which RFC 6570 section 2.3 allows. A key
// that addresses nothing as a path is therefore looked up once more as the
// plain name of a top-level argument, which is how such a property is read.
//
// A name the resource template bound out of the concrete uri is the exception:
// it is read under exactly that name and no path is resolved for it. The server
// derived that value from the uri it resolved and answers for, so it outranks
// anything the caller sent. Resolving "user.id" as a path first would let
// caller-supplied arguments {"user":{"id":"victim"}} answer in place of the
// variable the uri "users://42" bound, handing the handler an identity the
// request was never addressed to while the result still names users://42.
//
// Otherwise the path is resolved first, so arguments carrying both a nested
// "limit" object and a property literally named "limit.items" answer
// Get("limit.items") from the nested object: what a handler reads for a path it
// declared never depends on a peer adding a property whose name spells that
// path. The exact name is read with Request.Arg, which resolves no path at all.
//
// The second result reports whether a value was found. A found value may still
// be nil, because JSON null is a value.
func (r *Request) lookup(key string) (any, bool) {
	if r.args == nil {
		return nil, false
	}
	if _, bound := r.uriVars[key]; bound {
		v, ok := r.args[key]
		return v, ok
	}
	if v, ok := resolvePath(r.args, key); ok {
		return v, true
	}
	v, ok := r.args[key]
	return v, ok
}

// has reports whether key addresses an argument. It answers exactly when the
// accessors find a value, so a handler that sees Has report an argument can
// always read it with Get.
func (r *Request) has(key string) bool {
	_, ok := r.lookup(key)
	return ok
}

// ruleFieldFault reports why the validation engine cannot be trusted to check
// what the accessors read for field, or "" when it can. Request.Validate
// refuses a faulty field rather than running rules that would inspect another
// value, or no value at all, than the handler goes on to read.
//
// Two faults are possible. A "*" segment is a wildcard to the accessors and an
// ordinary member name to the engine, so the two never mean the same thing by
// it. Otherwise the engine, which walks nested objects only, may fail to reach
// a value the accessors resolve: an array element ("items.0"), a member of a
// container built in Go rather than decoded from JSON, an argument whose own
// name spells the field ("limit.items"), which no rule can address, or a
// resource template variable whose name carries a dot, which the accessors read
// under that name alone.
//
// The fault is reported before any rule runs, so a rule set that cannot check
// what the handler would read fails the call instead of reporting arguments
// validated. The refusal names the server's own rule, never the argument, and
// the client is told no more than that the call failed (see ErrRuleField).
func (r *Request) ruleFieldFault(field string) string {
	if hasWildcardSegment(field) {
		return `a "*" segment collects every element for the accessors and names one member for the validation engine`
	}
	ev, efound := engineValue(r.args, field)
	av, afound := r.lookup(field)
	if efound != afound || (efound && !reflect.DeepEqual(ev, av)) {
		return "the validation engine addresses members of nested objects only, so the rules would not see the value the accessors read"
	}
	return ""
}

// engineValue resolves field the way the velocity validation engine resolves a
// rule field: the field is split on dots and each leading segment must name a
// nested object. It reports presence separately, because a member that is
// present and null is a value the rules inspect.
func engineValue(args map[string]any, field string) (any, bool) {
	current := args
	parts := strings.Split(field, ".")
	for i, part := range parts {
		if i == len(parts)-1 {
			v, ok := current[part]
			return v, ok
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return nil, false
}

// resolvePath walks path through target one segment at a time.
func resolvePath(target any, path string) (any, bool) {
	// Each turn consumes one segment, so the walk is iterative and terminates
	// even for adversarially deep input.
	for {
		segment, rest, more := strings.Cut(path, ".")
		if segment == "*" {
			return collect(target, rest, more)
		}
		v, ok := member(target, segment)
		if !ok {
			return nil, false
		}
		if !more {
			return v, true
		}
		target, path = v, rest
	}
}

// member reads one named member of target: a map entry by key, or a slice
// element by decimal index. Anything else reports not found.
func member(target any, name string) (any, bool) {
	switch t := target.(type) {
	case map[string]any:
		v, ok := t[name]
		return v, ok
	case []any:
		i, ok := index(name, len(t))
		if !ok {
			return nil, false
		}
		return t[i], true
	default:
		return memberReflect(target, name)
	}
}

// memberReflect is the fallback for arguments built programmatically rather
// than decoded from JSON (a map[string]string, a []int, ...), keeping the
// accessors usable for hand-constructed requests such as those made by tests.
func memberReflect(target any, name string) (any, bool) {
	rv := reflect.ValueOf(target)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		v := rv.MapIndex(reflect.ValueOf(name).Convert(rv.Type().Key()))
		if !v.IsValid() {
			return nil, false
		}
		return v.Interface(), true
	case reflect.Slice, reflect.Array:
		i, ok := index(name, rv.Len())
		if !ok {
			return nil, false
		}
		return rv.Index(i).Interface(), true
	default:
		return nil, false
	}
}

// index parses a path segment as a slice index and bounds-checks it. Only a
// canonical decimal number is accepted, so "+1", "-1", " 1", "0x1" and "01" are
// member names rather than indices.
func index(segment string, length int) (int, bool) {
	if segment == "" {
		return 0, false
	}
	if len(segment) > 1 && segment[0] == '0' {
		return 0, false
	}
	for _, r := range segment {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	i, err := strconv.Atoi(segment)
	if err != nil || i >= length {
		return 0, false
	}
	return i, true
}

// collect resolves rest from every element of target, which is what a "*"
// segment means. more reports whether a separator followed the wildcard. The
// path ends where no separator follows, not where rest is empty: "items.*"
// collects the elements themselves, while "items.*." collects the member "" of
// each one, exactly as "items.0." reads it from the first, because the empty
// string is a legal JSON member name (RFC 8259 section 4). An element that does
// not carry the remaining path contributes a nil entry, so the result keeps
// positional correspondence with the input. A further "*" segment later in the
// path collapses one level, so "a.*.b.*.c" yields a single flat slice rather
// than a slice of slices.
func collect(target any, rest string, more bool) (any, bool) {
	elems, ok := elements(target)
	if !ok {
		return nil, false
	}
	out := make([]any, 0, len(elems))
	for _, e := range elems {
		if !more {
			out = append(out, e)
			continue
		}
		v, _ := resolvePath(e, rest)
		out = append(out, v)
	}
	if hasWildcardSegment(rest) {
		out = collapse(out)
	}
	return out, true
}

// elements lists the values a "*" segment iterates over. Object members are
// visited in sorted key order so the result of a wildcard lookup is stable
// across runs (Go map iteration order is deliberately randomised).
func elements(target any) ([]any, bool) {
	switch t := target.(type) {
	case []any:
		return t, true
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, t[k])
		}
		return out, true
	}

	// The same fallback member reads through: arguments built in Go rather than
	// decoded from JSON carry typed containers, and a wildcard has to walk
	// those too or a hand-built request would answer "headers.x" and not
	// "headers.*".
	rv := reflect.ValueOf(target)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out = append(out, rv.Index(i).Interface())
		}
		return out, true
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		return sortedMapValues(rv), true
	default:
		return nil, false
	}
}

// sortedMapValues lists a string-keyed map's values in sorted key order, the
// same order a decoded JSON object is visited in.
func sortedMapValues(rv reflect.Value) []any {
	type entry struct {
		key   string
		value any
	}
	entries := make([]entry, 0, rv.Len())
	iter := rv.MapRange()
	for iter.Next() {
		entries = append(entries, entry{key: iter.Key().String(), value: iter.Value().Interface()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.value)
	}
	return out
}

// hasWildcardSegment reports whether any segment of path is exactly "*". A
// member name that merely contains an asterisk ("ta*gs") is an ordinary name
// and does not collapse the result. Stopping at an empty remainder leaves at
// most a trailing empty segment unvisited, and that one is never a wildcard.
func hasWildcardSegment(path string) bool {
	for path != "" {
		var segment string
		segment, path, _ = strings.Cut(path, ".")
		if segment == "*" {
			return true
		}
	}
	return false
}

// collapse splices one level of nested lists into the outer list. An entry that
// is not itself a list is dropped: it is a hole left by an element that did not
// carry the wildcard path, and keeping it would misalign a result whose
// positions no longer correspond to the input anyway.
func collapse(values []any) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		if nested, ok := v.([]any); ok {
			out = append(out, nested...)
		}
	}
	return out
}
