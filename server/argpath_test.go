package server

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/velocitykode/velocity/validation"
)

// nestedArgs is the argument shape the dot-path tests read: a nested object, a
// list of objects, a list of lists, and a scalar.
func nestedArgs(t *testing.T) map[string]any {
	t.Helper()
	const raw = `{
		"user": {"name": "Alice", "email": "alice@example.com", "age": 30, "admin": true},
		"products": [{"name": "Widget"}, {"name": "Gadget"}],
		"orders": [{"lines": [{"sku": "a"}, {"sku": "b"}]}, {"lines": [{"sku": "c"}]}],
		"mixed": [{"lines": [{"sku": "a"}]}, {"note": "no lines here"}],
		"marks": [{"ta*gs": ["x", "y"]}, {"ta*gs": ["z"]}],
		"count": 2,
		"label": "plain"
	}`
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return args
}

func TestRequestGetDotPath(t *testing.T) {
	req := NewRequest(nestedArgs(t))

	cases := []struct {
		name string
		key  string
		want any
		has  bool
	}{
		{"nested member", "user.name", "Alice", true},
		{"nested missing member", "user.country", nil, false},
		{"array index", "products.0.name", "Widget", true},
		{"array index last", "products.1.name", "Gadget", true},
		{"array index out of range", "products.2.name", nil, false},
		{"array index negative", "products.-1.name", nil, false},
		{"array index not a number", "products.first.name", nil, false},
		{"array index leading zero", "products.01.name", nil, false},
		{"wildcard over array", "products.*.name", []any{"Widget", "Gadget"}, true},
		{"wildcard flattens nested wildcards", "orders.*.lines.*.sku", []any{"a", "b", "c"}, true},
		{"wildcard keeps holes", "orders.*.missing", []any{nil, nil}, true},
		// Collapsing a nested wildcard drops the elements that do not carry the
		// path; a flattened result has no positions to keep holes for.
		{"nested wildcard drops a missing intermediate", "mixed.*.lines.*.sku", []any{"a"}, true},
		// Only a segment that is exactly "*" collapses the result: an asterisk
		// inside a member name is part of the name.
		{"asterisk inside a member name is not a wildcard", "marks.*.ta*gs", []any{[]any{"x", "y"}, []any{"z"}}, true},
		{"wildcard over object", "user.*", []any{true, float64(30), "alice@example.com", "Alice"}, true},
		{"wildcard over scalar", "count.*", nil, false},
		{"path through scalar", "label.length", nil, false},
		{"path through missing root", "nope.name", nil, false},
		{"top level literal still works", "label", "plain", true},
		{"empty key", "", nil, false},
		{"lone dot", ".", nil, false},
		{"trailing dot", "user.", nil, false},
		{"double dot", "user..name", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := req.Get(tc.key)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Get(%q) = %#v, want %#v", tc.key, got, tc.want)
			}
			if has := req.Has(tc.key); has != tc.has {
				t.Fatalf("Has(%q) = %v, want %v", tc.key, has, tc.has)
			}
		})
	}
}

// A leading "*" collects every argument value, in sorted key order, exactly as
// it collects the members of a nested object.
func TestRequestLoneWildcardCollectsEveryArgument(t *testing.T) {
	req := NewRequest(map[string]any{"b": float64(2), "a": float64(1), "c": nil})
	if got := req.Get("*"); !reflect.DeepEqual(got, []any{float64(1), float64(2), nil}) {
		t.Fatalf("Get(*) = %#v, want every argument value in key order", got)
	}
	if !req.Has("*") {
		t.Fatal("Has(*) = false, want true")
	}

	if got := NewRequest(nil).Get("*"); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("Get(*) with no arguments = %#v, want an empty list", got)
	}
}

// A path that ends in a separator names the member "" of what precedes it: RFC
// 8259 section 4 lets a JSON object carry the empty string as a member name. A
// "*" segment keeps that last segment exactly as an index does, so "items.*."
// collects the member "" of every element where "items.*" collects the elements
// themselves. Reading the two alike hands a handler that asked for one member
// the whole of every element, whatever else the peer put beside it.
func TestRequestWildcardKeepsATrailingEmptySegment(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		key  string
		want []any
		// indexed spells the same read one element at a time, in the order the
		// wildcard visits them: each must answer what the wildcard collected at
		// that position.
		indexed []string
	}{
		{
			name:    "the empty member of every element",
			args:    map[string]any{"items": []any{map[string]any{"": float64(7), "secret": "x"}}},
			key:     "items.*.",
			want:    []any{float64(7)},
			indexed: []string{"items.0."},
		},
		{
			name:    "the elements themselves when no separator follows",
			args:    map[string]any{"items": []any{map[string]any{"": float64(7), "secret": "x"}}},
			key:     "items.*",
			want:    []any{map[string]any{"": float64(7), "secret": "x"}},
			indexed: []string{"items.0"},
		},
		{
			name:    "an element without the member leaves a hole",
			args:    map[string]any{"items": []any{map[string]any{"": float64(7)}, map[string]any{"secret": "x"}}},
			key:     "items.*.",
			want:    []any{float64(7), nil},
			indexed: []string{"items.0.", "items.1."},
		},
		{
			name:    "a scalar element carries no member",
			args:    map[string]any{"items": []any{float64(1), "two", nil}},
			key:     "items.*.",
			want:    []any{nil, nil, nil},
			indexed: []string{"items.0.", "items.1.", "items.2."},
		},
		{
			name:    "a member that is present and null",
			args:    map[string]any{"items": []any{map[string]any{"": nil, "secret": "x"}}},
			key:     "items.*.",
			want:    []any{nil},
			indexed: []string{"items.0."},
		},
		{
			name: "over the members of an object, in key order",
			args: map[string]any{"m": map[string]any{
				"b": map[string]any{"": float64(2), "secret": "x"},
				"a": map[string]any{"": float64(1)},
			}},
			key:     "m.*.",
			want:    []any{float64(1), float64(2)},
			indexed: []string{"m.a.", "m.b."},
		},
		{
			name: "over every argument",
			args: map[string]any{
				"x": map[string]any{"": float64(1)},
				"y": map[string]any{"": float64(2), "secret": "x"},
				"z": "scalar",
			},
			key:     "*.",
			want:    []any{float64(1), float64(2), nil},
			indexed: []string{"x.", "y.", "z."},
		},
		{
			name:    "two trailing empty members",
			args:    map[string]any{"items": []any{map[string]any{"": map[string]any{"": float64(9), "secret": "x"}}}},
			key:     "items.*..",
			want:    []any{float64(9)},
			indexed: []string{"items.0.."},
		},
		{
			name: "after a nested wildcard",
			args: map[string]any{"a": []any{
				[]any{map[string]any{"": float64(1)}, map[string]any{"": float64(2), "secret": "x"}},
				[]any{map[string]any{"": float64(3)}},
			}},
			key:     "a.*.*.",
			want:    []any{float64(1), float64(2), float64(3)},
			indexed: []string{"a.0.0.", "a.0.1.", "a.1.0."},
		},
		{
			name:    "an empty member before a further segment",
			args:    map[string]any{"items": []any{map[string]any{"": map[string]any{"id": float64(1), "secret": "x"}}}},
			key:     "items.*..id",
			want:    []any{float64(1)},
			indexed: []string{"items.0..id"},
		},
		{
			name:    "containers built in Go rather than decoded",
			args:    map[string]any{"headers": []map[string]string{{"": "v", "secret": "x"}}},
			key:     "headers.*.",
			want:    []any{"v"},
			indexed: []string{"headers.0."},
		},
		{
			name: "an empty list collects nothing",
			args: map[string]any{"items": []any{}},
			key:  "items.*.",
			want: []any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := NewRequest(tc.args)
			if !req.Has(tc.key) {
				t.Fatalf("Has(%q) = false, want true", tc.key)
			}
			if got := req.Get(tc.key); !reflect.DeepEqual(got, any(tc.want)) {
				t.Fatalf("Get(%q) = %#v, want %#v", tc.key, got, tc.want)
			}
			for i, key := range tc.indexed {
				if got := req.Get(key); !reflect.DeepEqual(got, tc.want[i]) {
					t.Fatalf("Get(%q) = %#v, want %#v: the wildcard and the index must read the same member", key, got, tc.want[i])
				}
			}
		})
	}
}

func TestRequestTypedGettersFollowDotPath(t *testing.T) {
	req := NewRequest(nestedArgs(t))

	if got, ok := req.StringOK("user.email"); got != "alice@example.com" || !ok {
		t.Fatalf("StringOK(user.email) = (%q,%v)", got, ok)
	}
	if got := req.String("products.1.name"); got != "Gadget" {
		t.Fatalf("String(products.1.name) = %q", got)
	}
	if got, ok := req.IntOK("user.age"); got != 30 || !ok {
		t.Fatalf("IntOK(user.age) = (%d,%v)", got, ok)
	}
	if got, ok := req.FloatOK("user.age"); got != 30 || !ok {
		t.Fatalf("FloatOK(user.age) = (%v,%v)", got, ok)
	}
	if got, ok := req.BoolOK("user.admin"); !got || !ok {
		t.Fatalf("BoolOK(user.admin) = (%v,%v)", got, ok)
	}

	// A path that resolves to the wrong type, or to nothing, reports not ok and
	// the zero value, exactly as a missing literal key does.
	if got, ok := req.StringOK("user.age"); got != "" || ok {
		t.Fatalf("StringOK(user.age) = (%q,%v), want (\"\",false)", got, ok)
	}
	if got, ok := req.StringOK("user.country"); got != "" || ok {
		t.Fatalf("StringOK(user.country) = (%q,%v), want (\"\",false)", got, ok)
	}
	if got, ok := req.BoolOK("user.name"); got || ok {
		t.Fatalf("BoolOK(user.name) = (%v,%v), want (false,false)", got, ok)
	}
	if got, ok := req.IntOK("products.0"); got != 0 || ok {
		t.Fatalf("IntOK(products.0) = (%d,%v), want (0,false)", got, ok)
	}
}

// A property declared with a dot in its name is legal input: the MCP
// specification restricts inputSchema property names in no way. A handler reads
// it under the name its schema declares, so a key that addresses nothing as a
// path is read as a plain argument name.
//
// When both spellings arrive the path wins and Arg reads the declared property,
// so what a handler reads for a path it wrote never depends on a peer adding a
// property whose name spells that path. A rule cannot address such a property
// at all, which is why validating one is the handler's own job; the Validate
// call at the end of this test states what happens to a rule that tries.
func TestRequestDottedNameIsReadByName(t *testing.T) {
	only := NewRequest(map[string]any{
		"limit.items": float64(5),
		"nested":      map[string]any{"a.b": "dotted name"},
	})

	if got, ok := only.Arg("limit.items"); got != float64(5) || !ok {
		t.Fatalf("Arg(limit.items) = (%#v,%v), want (5,true)", got, ok)
	}
	if got := only.All()["limit.items"]; got != float64(5) {
		t.Fatalf("All()[limit.items] = %#v, want the argument as it arrived", got)
	}
	// No nested limit object arrived, so the path addresses nothing and the
	// argument is read under the name the schema declared it with.
	if got := only.Get("limit.items"); got != float64(5) {
		t.Fatalf("Get(limit.items) = %#v, want the argument of that name", got)
	}
	if !only.Has("limit.items") {
		t.Fatal("Has(limit.items) = false while Get answers with the argument")
	}
	if got, ok := only.IntOK("limit.items"); got != 5 || !ok {
		t.Fatalf("IntOK(limit.items) = (%d,%v), want (5,true)", got, ok)
	}
	// The fallback reads an argument name, not a member name: a dotted member
	// of a nested object is reached by binding that object, not by path.
	if got := only.Get("nested.a.b"); got != nil {
		t.Fatalf("Get(nested.a.b) = %#v, want nil: only a top level name is read whole", got)
	}
	// Arg takes a name, not a path: a nested member is not one of its answers.
	if got, ok := only.Arg("nested.a.b"); got != nil || ok {
		t.Fatalf("Arg(nested.a.b) = (%#v,%v), want (nil,false)", got, ok)
	}

	// A peer that sends both spellings cannot decide which one a path reads.
	both := NewRequest(map[string]any{
		"limit.items": float64(5),
		"limit":       map[string]any{"items": float64(10)},
	})
	if got := both.Get("limit.items"); got != float64(10) {
		t.Fatalf("Get(limit.items) = %#v, want 10 from the nested object", got)
	}
	if got, ok := both.Arg("limit.items"); got != float64(5) || !ok {
		t.Fatalf("Arg(limit.items) = (%#v,%v), want (5,true)", got, ok)
	}
	if got := both.Get("limit"); !reflect.DeepEqual(got, map[string]any{"items": float64(10)}) {
		t.Fatalf("Get(limit) = %#v", got)
	}

	// A rule field is a path, and the validation engine resolves it as one: it
	// would report the property missing while the handler reads it. Rather than
	// claim a check it did not make, Validate refuses the field.
	err := only.Validate(validation.Rules{"limit.items": {validation.Required()}})
	if !errors.Is(err, ErrRuleField) {
		t.Fatalf("Validate(limit.items required) = %v, want a refusal: no rule can address the property", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Fatalf("Validate(limit.items required) = %v, want a rule error rather than a field error: the arguments are not at fault", err)
	}
}

// Has must report exactly what the accessors can read: a name Has accepts but
// Get answers with nil would make an argument look absent to every typed
// getter, and a name Has rejects but Get answers would hide one.
func TestRequestHasAgreesWithGet(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		key  string
		want any
		has  bool
	}{
		{"dotted name only", map[string]any{"limit.items": float64(5)}, "limit.items", float64(5), true},
		{"dotted name beside the path it spells", map[string]any{"limit.items": float64(5), "limit": map[string]any{"items": float64(10)}}, "limit.items", float64(10), true},
		{"path with no dotted name", map[string]any{"limit": map[string]any{"items": float64(10)}}, "limit.items", float64(10), true},
		// A name is read whole at the top level only: "h.a.b" is a member of
		// the arguments or nothing, never a member of the nested map.
		{"dotted name inside a nested map", map[string]any{"h": map[string]string{"a.b": "v"}}, "h.a.b", nil, false},
		// A member named "*" is collected like any other member: it cannot
		// replace the collection with itself, so the shape of a wildcard read
		// stays the handler's to decide rather than the peer's.
		{"a member named asterisk is collected, not obeyed", map[string]any{"*": float64(1), "b": float64(2)}, "*", []any{float64(1), float64(2)}, true},
		{"a nested member named asterisk is collected too", map[string]any{"a": map[string]any{"*": float64(1), "b": float64(2)}}, "a.*", []any{float64(1), float64(2)}, true},
		{"a member named asterisk does not replace a collected path", map[string]any{"items": map[string]any{"*.id": "injected", "a": map[string]any{"id": float64(1)}}}, "items.*.id", []any{nil, float64(1)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := NewRequest(tc.args)
			if has := req.Has(tc.key); has != tc.has {
				t.Fatalf("Has(%q) = %v, want %v", tc.key, has, tc.has)
			}
			if got := req.Get(tc.key); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Get(%q) = %#v, want %#v", tc.key, got, tc.want)
			}
		})
	}
}

// All reports the arguments as they arrived: it never expands or collapses dot
// paths, so a literal dotted property stays one entry.
func TestRequestAllIgnoresDotPaths(t *testing.T) {
	req := NewRequest(map[string]any{"limit.items": float64(5)})
	all := req.All()
	if len(all) != 1 || all["limit.items"] != float64(5) {
		t.Fatalf("All() = %#v", all)
	}
}

func TestRequestDotPathHostileKeys(t *testing.T) {
	req := NewRequest(map[string]any{
		"user":     map[string]any{"na\x00me": "nul", "naïve": "unicode", "a\nb": "newline"},
		"nul\x00k": "literal nul",
		"emoji":    map[string]any{"🙂": "smile"},
	})

	cases := []struct {
		key  string
		want any
	}{
		{"user.na\x00me", "nul"},
		{"user.naïve", "unicode"},
		{"user.a\nb", "newline"},
		{"nul\x00k", "literal nul"},
		{"emoji.🙂", "smile"},
		{"user.NAÏVE", nil}, // key comparison is exact, never case folded
	}
	for _, tc := range cases {
		if got := req.Get(tc.key); got != tc.want {
			t.Fatalf("Get(%q) = %#v, want %#v", tc.key, got, tc.want)
		}
	}
}

// Arguments are not always decoded from JSON: templated resource reads and
// hand-built requests supply typed Go maps and slices, and those must walk too.
func TestRequestDotPathTypedContainers(t *testing.T) {
	req := NewRequest(map[string]any{
		"headers": map[string]string{"x-trace": "abc"},
		"ids":     []int{7, 8},
		"pairs":   [2]string{"first", "second"},
	})

	if got := req.Get("headers.x-trace"); got != "abc" {
		t.Fatalf("Get(headers.x-trace) = %#v", got)
	}
	if got, ok := req.IntOK("ids.1"); got != 8 || !ok {
		t.Fatalf("IntOK(ids.1) = (%d,%v)", got, ok)
	}
	if got := req.Get("pairs.0"); got != "first" {
		t.Fatalf("Get(pairs.0) = %#v", got)
	}
	if got, ok := req.Get("ids.*"), req.Has("ids.*"); !reflect.DeepEqual(got, []any{7, 8}) || !ok {
		t.Fatalf("Get(ids.*) = %#v (has=%v)", got, ok)
	}
	if got := req.Get("headers.missing"); got != nil {
		t.Fatalf("Get(headers.missing) = %#v", got)
	}
}

// A wildcard over a typed map must answer exactly as it does over a decoded
// JSON object: in sorted key order, and reporting the path as present. Without
// it a hand-built request would resolve "headers.x-trace" and then report
// "headers.*" as absent, which is the shape a templated resource read and every
// test fixture arrive in.
func TestRequestWildcardOverTypedMaps(t *testing.T) {
	type headerName string

	tests := []struct {
		name string
		args map[string]any
		key  string
		want []any
	}{
		{
			name: "a string-valued map is visited in key order",
			args: map[string]any{"headers": map[string]string{"b": "second", "a": "first", "c": "third"}},
			key:  "headers.*",
			want: []any{"first", "second", "third"},
		},
		{
			name: "a map with a named string key type walks too",
			args: map[string]any{"headers": map[headerName]string{"b": "second", "a": "first"}},
			key:  "headers.*",
			want: []any{"first", "second"},
		},
		{
			name: "an integer-valued map keeps its values typed",
			args: map[string]any{"counts": map[string]int{"z": 2, "a": 1}},
			key:  "counts.*",
			want: []any{1, 2},
		},
		{
			name: "a wildcard reaches into the members it collects",
			args: map[string]any{"users": map[string]map[string]any{
				"b": {"name": "Bea"},
				"a": {"name": "Ada"},
			}},
			key:  "users.*.name",
			want: []any{"Ada", "Bea"},
		},
		{
			name: "an empty typed map collects nothing but is still present",
			args: map[string]any{"headers": map[string]string{}},
			key:  "headers.*",
			want: []any{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := NewRequest(tc.args)
			got, ok := req.Get(tc.key), req.Has(tc.key)
			if !ok {
				t.Fatalf("Has(%q) = false, want true", tc.key)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Get(%q) = %#v, want %#v", tc.key, got, tc.want)
			}
			// Repeating the lookup must give the same answer: Go randomises map
			// iteration, so an unsorted walk would only fail some of the time.
			for range 20 {
				if again := req.Get(tc.key); !reflect.DeepEqual(again, tc.want) {
					t.Fatalf("Get(%q) = %#v on a repeat, want %#v", tc.key, again, tc.want)
				}
			}
		})
	}
}

// A map the accessors cannot address by name cannot be walked by a wildcard
// either: the two must agree on what a container is.
func TestRequestWildcardRejectsNonStringKeyedMaps(t *testing.T) {
	req := NewRequest(map[string]any{"counts": map[int]string{1: "one"}})
	if got, ok := req.Get("counts.*"), req.Has("counts.*"); got != nil || ok {
		t.Fatalf("Get(counts.*) = %#v (has=%v), want nil and false", got, ok)
	}
}

// A null argument is present: the accessors must distinguish "absent" from
// "explicitly null" along a path just as they do for a literal key.
func TestRequestDotPathNullValue(t *testing.T) {
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"user":{"name":null}}`), &args); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req := NewRequest(args)
	if !req.Has("user.name") {
		t.Fatal("Has(user.name) = false, want true for an explicit null")
	}
	if got := req.Get("user.name"); got != nil {
		t.Fatalf("Get(user.name) = %#v, want nil", got)
	}
	if got, ok := req.StringOK("user.name"); got != "" || ok {
		t.Fatalf("StringOK(user.name) = (%q,%v), want (\"\",false)", got, ok)
	}
}

// Deeply nested input must not blow the stack or hang: resolution walks
// iteratively, one segment at a time.
func TestRequestDotPathDeepNesting(t *testing.T) {
	const depth = 5000
	leaf := map[string]any{"leaf": "bottom"}
	path := "leaf"
	node := leaf
	for i := 0; i < depth; i++ {
		node = map[string]any{"n": node}
		path = "n." + path
	}
	req := NewRequest(node)
	if got := req.Get(path); got != "bottom" {
		t.Fatalf("deep Get = %#v, want \"bottom\"", got)
	}
	if got := req.Get(path + ".more"); got != nil {
		t.Fatalf("deep Get past the leaf = %#v, want nil", got)
	}
}

// Reading a request from several handlers at once must be race free: a Request
// is read-only once the method handler has built it.
func TestRequestDotPathConcurrentReads(t *testing.T) {
	req := NewRequest(nestedArgs(t))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := req.String("user.name"); got != "Alice" {
					t.Errorf("String(user.name) = %q", got)
					return
				}
				if got, ok := req.Get("products.*.name").([]any); !ok || len(got) != 2 {
					t.Errorf("Get(products.*.name) = %#v", got)
					return
				}
				if !req.Has("orders.0.lines.1.sku") {
					t.Error("Has(orders.0.lines.1.sku) = false")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// checkWildcardAgreesWithIndexedReads holds a key with one "*" segment to what
// that segment means: the entry collected at a position is the value the same
// key reads with the position spelled out, so Get("items.*.id")[1] is
// Get("items.1.id") and Get("items.*.")[0] is Get("items.0."). It checks nothing
// where the relation cannot be spelled: a second wildcard flattens the result,
// and a name that carries a dot or is "*" cannot be written as one segment.
func checkWildcardAgreesWithIndexedReads(t *testing.T, req *Request, key string) {
	t.Helper()

	segments := strings.Split(key, ".")
	at := -1
	for i, segment := range segments {
		if segment != "*" {
			continue
		}
		if at >= 0 {
			return
		}
		at = i
	}
	if at < 0 {
		return
	}
	// A key that addresses nothing as a path is read as the plain name of an
	// argument. With no dotted argument name that cannot happen to a dotted
	// key, which keeps every read below a path read.
	for name := range req.All() {
		if strings.Contains(name, ".") {
			return
		}
	}

	var container any = req.All()
	if at > 0 {
		container = req.Get(strings.Join(segments[:at], "."))
	}
	var names []string
	switch c := container.(type) {
	case []any:
		for i := range c {
			names = append(names, strconv.Itoa(i))
		}
	case map[string]any:
		for name := range c {
			if name == "*" || strings.Contains(name, ".") {
				return
			}
			names = append(names, name)
		}
		sort.Strings(names)
	default:
		if req.Has(key) {
			t.Fatalf("Has(%q) = true with no container for the wildcard to collect from", key)
		}
		return
	}

	got, ok := req.Get(key).([]any)
	if !ok || len(got) != len(names) {
		t.Fatalf("Get(%q) = %#v, want one entry for each of the %d elements", key, req.Get(key), len(names))
	}
	for i, name := range names {
		indexed := append(append(append([]string{}, segments[:at]...), name), segments[at+1:]...)
		one := strings.Join(indexed, ".")
		if want := req.Get(one); !reflect.DeepEqual(got[i], want) {
			t.Fatalf("Get(%q)[%d] = %#v, but Get(%q) = %#v", key, i, got[i], one, want)
		}
	}
}

// FuzzRequestLookup drives the accessors with arbitrary decoded JSON arguments
// and arbitrary keys: both come from an untrusted peer, so no input may panic,
// Has must stay consistent with the typed getters, a "*" segment must collect
// what the same key reads one element at a time, and a key that passes its
// validation rules must read back the value those rules checked.
func FuzzRequestLookup(f *testing.F) {
	seeds := []struct {
		args string
		key  string
	}{
		{`{"user":{"name":"Alice"}}`, "user.name"},
		{`{"limit.items":5,"limit":{"items":10}}`, "limit.items"},
		{`{"products":[{"name":"Widget"}]}`, "products.*.name"},
		{`{"a":[[1,2],[3]]}`, "a.*.*"},
		{`{"a":{"b":{"c":null}}}`, "a.b.c"},
		{`{"a":[1,2,3]}`, "a.-1"},
		{`{"a":[1,2,3]}`, "a.999999999999999999999"},
		{`{"":{"":1}}`, "."},
		{`{"a":1}`, "a..b"},
		{`{"a":1}`, "*"},
		{"{\"\\u0000\":1}", "\x00"},
		{`[]`, "a"},
		{`{"limit.items":5}`, "limit.items"},
		{`{"a.b.c":1,"a":{"b":{"c":2}}}`, "a.b.c"},
		{`{"*.x":1,"a":{"x":2}}`, "*.x"},
		{`{"user":{"name":"ok"},"user.name":"AAAAAAAAAA"}`, "user.name"},
		{`{"user":{"name":"AAAAAAAAAA"},"user.name":"ok"}`, "user.name"},
		{`{"a":{"b":{"c":"ok"}},"a.b.c":"AAAAAAAAAA","a.b":{"c":"AAAAAAAAAA"}}`, "a.b.c"},
		// Keys the validation engine and the accessors read differently: a
		// member literally named "*", and an array element.
		{`{"*":"ok"}`, "*"},
		{`{"items":{"*":"ok","z":"AAAAAAAAAA"}}`, "items.*"},
		{`{"items":["AAAAAAAAAA"]}`, "items.0"},
		// A separator after a wildcard leaves an empty last segment, which names
		// the member "" of every element rather than ending the path.
		{`{"items":[{"":7,"secret":"AAAAAAAAAA"}]}`, "items.*."},
		{`{"items":[{"":7,"secret":"AAAAAAAAAA"}]}`, "items.0."},
		{`{"items":{"b":{"":2},"a":{"":1,"secret":"AAAAAAAAAA"}}}`, "items.*."},
		{`{"x":{"":1},"y":"AAAAAAAAAA"}`, "*."},
		{`{"items":[{"":{"":9}}]}`, "items.*.."},
		{`{"a":[[{"":1}],[{"":2,"secret":"AAAAAAAAAA"}]]}`, "a.*.*."},
	}
	for _, s := range seeds {
		f.Add(s.args, s.key)
	}

	f.Fuzz(func(t *testing.T, argsJSON, key string) {
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return // only well-formed argument objects reach a Request
		}
		req := NewRequest(args)

		has := req.Has(key)
		got := req.Get(key)
		if !has && got != nil {
			t.Fatalf("Get(%q) = %#v while Has reported absent", key, got)
		}
		checkWildcardAgreesWithIndexedReads(t, req, key)

		// Whatever else the peer sent, a key whose rules ran and passed reads
		// back the value those rules saw: the rules bound the value to at most
		// 5 bytes, so a longer one means the accessors resolved the key to
		// something the validation engine never inspected. A key the engine
		// cannot address as the accessors do is refused instead, and then
		// nothing ran and nothing is claimed.
		rules := validation.Rules{key: {validation.Required(), validation.String(), validation.Max(5)}}
		switch err := req.Validate(rules); {
		case errors.Is(err, ErrRuleField):
		case err == nil:
			if s, ok := req.StringOK(key); !ok {
				t.Fatalf("StringOK(%q) found nothing for a key that passed a required string rule", key)
			} else if len(s) > 5 {
				t.Fatalf("String(%q) = %d bytes after rules bounding it to 5: the accessors read a value the rules never saw", key, len(s))
			}
		}

		s, sok := req.StringOK(key)
		if sok {
			if !has {
				t.Fatalf("StringOK(%q) found a value Has does not report", key)
			}
			if got != any(s) {
				t.Fatalf("StringOK(%q) = %q but Get returned %#v", key, s, got)
			}
		}
		if n, ok := req.IntOK(key); ok {
			if !has {
				t.Fatalf("IntOK(%q) found a value Has does not report", key)
			}
			if f, _ := req.FloatOK(key); f != float64(n) {
				t.Fatalf("IntOK/FloatOK disagree for %q: %d vs %v", key, n, f)
			}
		}
		if b, ok := req.BoolOK(key); ok && got != any(b) {
			t.Fatalf("BoolOK(%q) = %v but Get returned %#v", key, b, got)
		}
	})
}
