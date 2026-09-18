package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/velocitykode/velocity/validation"
)

func TestRequestTypedGetters(t *testing.T) {
	req := NewRequest(map[string]any{
		"name":  "ada",
		"n":     float64(7),
		"x":     3.5,
		"ok":    true,
		"whole": float64(42),
		"frac":  2.5,
	})

	tests := []struct {
		name string
		fn   func() (any, bool)
		val  any
		ok   bool
	}{
		{"string present", func() (any, bool) { v, ok := req.StringOK("name"); return v, ok }, "ada", true},
		{"string missing", func() (any, bool) { v, ok := req.StringOK("nope"); return v, ok }, "", false},
		{"string wrong type", func() (any, bool) { v, ok := req.StringOK("ok"); return v, ok }, "", false},
		{"float present", func() (any, bool) { v, ok := req.FloatOK("x"); return v, ok }, 3.5, true},
		{"float from int", func() (any, bool) { v, ok := req.FloatOK("n"); return v, ok }, 7.0, true},
		{"float missing", func() (any, bool) { v, ok := req.FloatOK("nope"); return v, ok }, 0.0, false},
		{"int whole", func() (any, bool) { v, ok := req.IntOK("whole"); return v, ok }, int64(42), true},
		{"int fractional", func() (any, bool) { v, ok := req.IntOK("frac"); return v, ok }, int64(0), false},
		{"bool present", func() (any, bool) { v, ok := req.BoolOK("ok"); return v, ok }, true, true},
		{"bool wrong type", func() (any, bool) { v, ok := req.BoolOK("name"); return v, ok }, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok := tt.fn()
			if v != tt.val || ok != tt.ok {
				t.Fatalf("got (%v,%v) want (%v,%v)", v, ok, tt.val, tt.ok)
			}
		})
	}

	// Plain getters return zero values for missing keys.
	if req.String("nope") != "" || req.Int("nope") != 0 || req.Float("nope") != 0 || req.Bool("nope") {
		t.Fatal("plain getters should return zero values for missing keys")
	}
	if req.String("name") != "ada" || req.Int("whole") != 42 || req.Float("x") != 3.5 || !req.Bool("ok") {
		t.Fatal("plain getters returned wrong values")
	}
}

func TestRequestMetadataAccessors(t *testing.T) {
	req := NewRequest(map[string]any{"a": 1}).
		WithSessionID("sess-1").
		WithMeta(map[string]any{"trace": "abc"}).
		WithURI("file://x")

	if req.SessionID() != "sess-1" {
		t.Fatalf("session id = %q", req.SessionID())
	}
	if req.URI() != "file://x" {
		t.Fatalf("uri = %q", req.URI())
	}
	if req.Meta()["trace"] != "abc" {
		t.Fatalf("meta = %v", req.Meta())
	}
	if !req.Has("a") || req.Has("b") {
		t.Fatal("Has reported wrong presence")
	}
	if req.Get("a") != 1 {
		t.Fatal("Get returned wrong value")
	}

	all := req.All()
	all["a"] = 999 // mutating the copy must not affect the request
	if req.Get("a") != 1 {
		t.Fatal("All returned a non-copy")
	}
}

func TestRequestNilArgs(t *testing.T) {
	req := NewRequest(nil)
	if req.All() == nil {
		t.Fatal("All should be non-nil for nil args")
	}
	if len(req.All()) != 0 {
		t.Fatal("All should be empty for nil args")
	}
}

type bindTarget struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func TestRequestBind(t *testing.T) {
	req := NewRequest(map[string]any{"name": "ada", "n": float64(7)})
	var dst bindTarget
	if err := req.Bind(&dst); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if dst.Name != "ada" || dst.N != 7 {
		t.Fatalf("bound = %+v", dst)
	}
}

func TestRequestBind_EdgeCase(t *testing.T) {
	req := NewRequest(map[string]any{"n": "not-a-number"})
	var dst bindTarget
	if err := req.Bind(&dst); err == nil {
		t.Fatal("expected bind error for type mismatch")
	}
}

func TestRequestValidate(t *testing.T) {
	req := NewRequest(map[string]any{"name": "ada", "email": "ada@example.com"})
	err := req.Validate(validation.Rules{
		"name":  {validation.Required()},
		"email": {validation.Required(), validation.Email()},
	})
	if err != nil {
		t.Fatalf("validate passed should be nil: %v", err)
	}
}

func TestRequestValidate_EdgeCase(t *testing.T) {
	req := NewRequest(map[string]any{"name": ""})
	err := req.Validate(validation.Rules{"name": {validation.Required()}})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error should wrap ErrValidation: %v", err)
	}
}

// A handler validates a key and then reads it, so the two must resolve to the
// same value for every key a peer can send. The validation engine splits a rule
// field on dots and walks the nested objects; an accessor that answered a
// differently resolved value would hand the handler input no rule ever saw.
func TestRequestValidateAndAccessorsResolveTheSameKey(t *testing.T) {
	// Long enough to fail the max rule by a wide margin, so a value that slips
	// past it is unmistakable.
	oversized := strings.Repeat("A", 5000)

	cases := []struct {
		name string
		// args is the arguments object as a peer could send it.
		args map[string]any
		// wantErr is whether the rules below must reject those arguments as a
		// field failure the client is told about.
		wantErr bool
		// wantRefused is whether the rule field itself must be refused, because
		// the engine cannot address the value the accessors would read.
		wantRefused bool
		// wantRead is what String must answer for the validated key once the
		// rules have passed.
		wantRead string
	}{
		{
			name:     "nested value only",
			args:     map[string]any{"user": map[string]any{"name": "ok"}},
			wantRead: "ok",
		},
		{
			// The peer adds a property whose name spells the validated path.
			// The rules check the nested value, so the accessors must read the
			// nested value too.
			name:     "a dotted name beside the validated path",
			args:     map[string]any{"user": map[string]any{"name": "ok"}, "user.name": oversized},
			wantRead: "ok",
		},
		{
			// The mirror image: an oversized nested value cannot be hidden
			// behind a short property of the dotted name.
			name:    "an oversized nested value behind a short dotted name",
			args:    map[string]any{"user": map[string]any{"name": oversized}, "user.name": "ok"},
			wantErr: true,
		},
		{
			// The handler reads the property under its own name, and no rule
			// can address it, so the call is refused rather than reported
			// validated on a value the rules never saw.
			name:        "a dotted name and no nested object",
			args:        map[string]any{"user.name": "ok"},
			wantRefused: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := NewRequest(tc.args)
			err := req.Validate(validation.Rules{
				"user.name": {validation.Required(), validation.String(), validation.Max(5)},
			})
			if tc.wantRefused {
				if !errors.Is(err, ErrRuleField) {
					t.Fatalf("Validate = %v, want a refusal wrapping ErrRuleField", err)
				}
				return
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("Validate = nil, want these arguments rejected")
				}
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("error should wrap ErrValidation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
			// The unvalidated values in this table are thousands of bytes
			// long, so a failure reports what was read by length, never by
			// printing it.
			if got := req.String("user.name"); got != tc.wantRead {
				t.Fatalf("String(user.name) read %d bytes, want the validated value %q", len(got), tc.wantRead)
			}
			if got, ok := req.Get("user.name").(string); !ok || got != tc.wantRead {
				t.Fatalf("Get(user.name) read a %T of %d bytes, want the validated value %q", req.Get("user.name"), len(got), tc.wantRead)
			}
		})
	}
}

// The validation engine addresses members of nested objects; the accessors also
// index arrays and collect wildcards. Where the two differ, running the rules
// would tell the handler a value was checked when the value it then reads was
// seen by no rule. Validate refuses such a field instead, before any rule runs.
func TestRequestValidateRefusesFieldsTheRulesCannotReach(t *testing.T) {
	// Long enough that a failure is unmistakable, and never printed.
	oversized := strings.Repeat("A", 5000)

	cases := []struct {
		name string
		args map[string]any
		// field is the single rule field the case declares.
		field string
		// rules are the rules on that field; the zero value means a required
		// string of at most 5 bytes.
		rules []validation.Rule
		// wantRefused is whether Validate must refuse the field rather than
		// answer with a validation result.
		wantRefused bool
		// wantValid is what Validate must answer for a field it accepts.
		wantValid bool
	}{
		{
			// The engine reads "*" as a member name and finds the short value;
			// the accessors collect every member, oversized one included.
			name:        "a wildcard segment",
			args:        map[string]any{"items": map[string]any{"*": "ok", "z": oversized}},
			field:       "items.*",
			wantRefused: true,
		},
		{
			name:        "a wildcard as the whole field",
			args:        map[string]any{"*": "ok"},
			field:       "*",
			wantRefused: true,
		},
		{
			// The engine does not walk into an array, so it treats the element
			// as absent and skips every rule but required.
			name:        "an array element",
			args:        map[string]any{"items": []any{oversized}},
			field:       "items.0",
			wantRefused: true,
		},
		{
			// The same element under rules that do not require it. Nothing
			// reports the field missing here, so an unreachable field would
			// leave the rules silently skipped and the oversized element read
			// as if it had been checked.
			name:        "an array element under optional rules",
			args:        map[string]any{"items": []any{oversized}},
			field:       "items.0",
			rules:       []validation.Rule{validation.Nullable(), validation.String(), validation.Max(5)},
			wantRefused: true,
		},
		{
			name:        "a member of an object inside an array",
			args:        map[string]any{"items": []any{map[string]any{"name": oversized}}},
			field:       "items.0.name",
			wantRefused: true,
		},
		{
			// Arguments built in Go rather than decoded from JSON: the
			// accessors read the typed map, the engine does not.
			name:        "a member of a container the engine does not walk",
			args:        map[string]any{"headers": map[string]string{"trace": oversized}},
			field:       "headers.trace",
			wantRefused: true,
		},
		{
			name:      "a member of a nested object",
			args:      map[string]any{"user": map[string]any{"name": "ok"}},
			field:     "user.name",
			wantValid: true,
		},
		{
			// A numeric segment is only an index when the container is an
			// array: here it names a member, which both resolve alike.
			name:      "a numeric member name of an object",
			args:      map[string]any{"items": map[string]any{"0": "ok"}},
			field:     "items.0",
			wantValid: true,
		},
		{
			// A separator after the wildcard adds one more segment, the member
			// "" of every element. The accessors collect it and the engine
			// looks for a member named "*", so the field is still refused.
			name:        "the empty member after a wildcard segment",
			args:        map[string]any{"items": map[string]any{"*": map[string]any{"": "ok"}, "z": map[string]any{"": oversized}}},
			field:       "items.*.",
			wantRefused: true,
		},
		{
			name:        "the empty member of an array element",
			args:        map[string]any{"items": []any{map[string]any{"": oversized}}},
			field:       "items.0.",
			wantRefused: true,
		},
		{
			// Both split the field on dots, so both read the member "" of the
			// nested object.
			name:      "the empty member of a nested object",
			args:      map[string]any{"user": map[string]any{"": "ok"}},
			field:     "user.",
			wantValid: true,
		},
		{
			name:  "the empty member of a nested object that is too long",
			args:  map[string]any{"user": map[string]any{"": oversized}},
			field: "user.",
		},
		{
			name:  "a nested object the arguments do not carry",
			args:  map[string]any{"other": "ok"},
			field: "user.name",
		},
		{
			name:  "a member of a nested object that is too long",
			args:  map[string]any{"user": map[string]any{"name": oversized}},
			field: "user.name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := tc.rules
			if rules == nil {
				rules = []validation.Rule{validation.Required(), validation.String(), validation.Max(5)}
			}
			req := NewRequest(tc.args)
			err := req.Validate(validation.Rules{tc.field: rules})

			if tc.wantRefused {
				if !errors.Is(err, ErrRuleField) {
					t.Fatalf("Validate(%q) = %v, want a refusal wrapping ErrRuleField", tc.field, err)
				}
				// A refusal is a fault in the rules, not a client field error:
				// reporting it as one would tell the peer to fix its input.
				if errors.Is(err, ErrValidation) {
					t.Fatalf("Validate(%q) = %v, want a rule error rather than a validation failure", tc.field, err)
				}
				if !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("Validate(%q) = %v, want the error to name the field", tc.field, err)
				}
				return
			}
			if errors.Is(err, ErrRuleField) {
				t.Fatalf("Validate(%q) = %v, want the field checked: the rules address the value the accessors read", tc.field, err)
			}
			if tc.wantValid {
				if err != nil {
					t.Fatalf("Validate(%q) = %v, want nil", tc.field, err)
				}
				return
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate(%q) = %v, want a validation failure", tc.field, err)
			}
		})
	}
}

// A refusal must stop the call. The invocation path turns a validation failure
// into a tool-level error result the client is expected to act on, but a rule
// that cannot check what the handler reads is the server's own fault: the call
// fails and the client is told nothing beyond a generic internal error, rather
// than being handed a result built from a value no rule inspected.
func TestInvokeToolFailsWhenRulesCannotReachTheField(t *testing.T) {
	unchecked := strings.Repeat("A", 5000)

	tool := NewTool("first-item", "reads the first item").
		HandleFunc(func(_ context.Context, req *Request) (*Response, error) {
			if err := req.Validate(validation.Rules{
				"items.0": {validation.Nullable(), validation.String(), validation.Max(5)},
			}); err != nil {
				return nil, err
			}
			return Text(req.String("items.0")), nil
		})

	req := NewRequest(map[string]any{"items": []any{unchecked}})
	result, err := InvokeTool(context.Background(), tool, req)
	if !errors.Is(err, ErrRuleField) {
		t.Fatalf("InvokeTool = (%v, %v), want the call to fail with ErrRuleField", result != nil, err)
	}
	if result != nil {
		t.Fatal("InvokeTool returned a result for a call whose rules checked nothing")
	}
}

// A refused field must stay refused whichever field of the rule set the engine
// happens to visit first: the report names the same field every run.
func TestRequestValidateNamesTheSameRefusedFieldEveryRun(t *testing.T) {
	req := NewRequest(map[string]any{"items": []any{"a"}, "tags": []any{"b"}})
	rules := validation.Rules{
		"name":    {validation.Required()},
		"items.0": {validation.Required()},
		"tags.0":  {validation.Required()},
	}
	first := req.Validate(rules)
	if !errors.Is(first, ErrRuleField) {
		t.Fatalf("Validate = %v, want a refusal wrapping ErrRuleField", first)
	}
	for range 50 {
		if again := req.Validate(rules); again.Error() != first.Error() {
			t.Fatalf("Validate reported %v, having reported %v: the refusal follows map iteration order", again, first)
		}
	}
}

// engineValue reproduces how the validation engine resolves a rule field, and
// everything Validate promises rests on that reproduction staying faithful: a
// field the two agree on runs its rules, a field they differ on is refused. The
// engine belongs to the framework and can change under this module, so the
// fixtures below are resolved by the engine itself and compared against the
// copy, value and presence alike. A framework release that addresses a field
// differently fails here, where the copy is fixed, rather than silently where
// the guarantee is made.
func TestEngineValueResolvesAFieldLikeTheValidationEngine(t *testing.T) {
	cases := []struct {
		name  string
		args  map[string]any
		field string
	}{
		{"a top level argument", map[string]any{"name": "Alice"}, "name"},
		{"a member of a nested object", map[string]any{"user": map[string]any{"name": "Alice"}}, "user.name"},
		{"a member the nested object does not carry", map[string]any{"user": map[string]any{"name": "Alice"}}, "user.country"},
		{"a member of an object that did not arrive", map[string]any{"other": 1}, "user.name"},
		{"a member that is present and null", map[string]any{"user": map[string]any{"name": nil}}, "user.name"},
		{"an element of an array", map[string]any{"items": []any{"first"}}, "items.0"},
		{"a member of an object inside an array", map[string]any{"items": []any{map[string]any{"name": "first"}}}, "items.0.name"},
		{"a numeric member name of an object", map[string]any{"items": map[string]any{"0": "first"}}, "items.0"},
		{"an argument whose own name spells the field", map[string]any{"user.name": "Alice"}, "user.name"},
		{"both spellings at once", map[string]any{"user.name": "literal", "user": map[string]any{"name": "nested"}}, "user.name"},
		{"a member of a container built in Go", map[string]any{"headers": map[string]string{"trace": "abc"}}, "headers.trace"},
		{"a wildcard segment", map[string]any{"items": map[string]any{"*": "star", "z": "other"}}, "items.*"},
		{"a segment through a scalar", map[string]any{"label": "plain"}, "label.length"},
		{"an empty field", map[string]any{"": 1}, ""},
		// An empty segment names the member "" (RFC 8259 section 4 allows the
		// empty string as a member name), wherever in the field it falls.
		{"a trailing empty segment", map[string]any{"user": map[string]any{"": "anonymous"}}, "user."},
		{"a trailing empty segment the object does not carry", map[string]any{"user": map[string]any{"name": "Alice"}}, "user."},
		{"a leading empty segment", map[string]any{"": map[string]any{"name": "Alice"}}, ".name"},
		{"an empty segment between two others", map[string]any{"user": map[string]any{"": map[string]any{"name": "Alice"}}}, "user..name"},
		{"a lone separator", map[string]any{"": map[string]any{"": "nested"}}, "."},
		{"an empty segment after an array element", map[string]any{"items": []any{map[string]any{"": "first"}}}, "items.0."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The engine hands every rule the value it resolved for the field,
			// including nil for a field it did not reach, so a rule that
			// records its value reports that resolution exactly.
			var seen any
			var ran bool
			record := validation.Custom("record_resolved_value", func(_ string, value any, _ []string, _ map[string]any) error {
				seen, ran = value, true
				return nil
			})
			if _, err := validation.NewValidator().Validate(tc.args, validation.Rules{tc.field: {record}}); err != nil {
				t.Fatalf("the engine failed on the recording rule: %v", err)
			}
			if !ran {
				t.Fatal("the engine ran no rule for the field, so its resolution was not observed")
			}

			got, found := engineValue(tc.args, tc.field)
			if !reflect.DeepEqual(seen, got) {
				t.Fatalf("engineValue(%q) = %#v, the engine resolved %#v", tc.field, got, seen)
			}

			// Presence is separate: the engine reports a value it did not reach
			// and one that arrived as null alike, and only a presence rule
			// tells the two apart.
			_, perr := validation.NewValidator().Validate(tc.args, validation.Rules{tc.field: {validation.Present()}})
			if present := perr == nil; present != found {
				t.Fatalf("engineValue(%q) reports present = %v, the engine reports %v", tc.field, found, present)
			}
		})
	}
}

// A rule may name a second field as a parameter. The validation engine resolves
// that name itself, so what Validate answers for such a rule set is pinned here
// rather than inferred: a top level argument named by the rule is read, and a
// mismatch is a field failure the client is told about.
func TestRequestValidateComparesAgainstASecondField(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		rules   validation.Rules
		wantErr bool
	}{
		{
			name:  "a matching second argument",
			args:  map[string]any{"token": "abc", "token_confirmation": "abc"},
			rules: validation.Rules{"token": {validation.Required(), validation.Same("token_confirmation")}},
		},
		{
			name:    "a second argument that differs",
			args:    map[string]any{"token": "abc", "token_confirmation": "xyz"},
			rules:   validation.Rules{"token": {validation.Required(), validation.Same("token_confirmation")}},
			wantErr: true,
		},
		{
			name:    "no second argument to compare with",
			args:    map[string]any{"token": "abc"},
			rules:   validation.Rules{"token": {validation.Required(), validation.Same("token_confirmation")}},
			wantErr: true,
		},
		{
			name:    "required because a second argument says so",
			args:    map[string]any{"mode": "manual"},
			rules:   validation.Rules{"reason": {validation.RequiredIf("mode", "manual")}},
			wantErr: true,
		},
		{
			name:  "not required because the second argument says otherwise",
			args:  map[string]any{"mode": "auto"},
			rules: validation.Rules{"reason": {validation.RequiredIf("mode", "manual")}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewRequest(tc.args).Validate(tc.rules)
			if errors.Is(err, ErrRuleField) {
				t.Fatalf("Validate = %v, want the rules run: every field here is one the engine addresses", err)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("Validate = %v, want a validation failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
		})
	}
}

func TestToFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2), 2, true},
		{int(3), 3, true},
		{int64(4), 4, true},
		{uint(5), 5, true},
		{json.Number("6.5"), 6.5, true},
		{json.Number("bad"), 0, false},
		{"str", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toFloat(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("toFloat(%v) = (%v,%v) want (%v,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestReportProgress(t *testing.T) {
	var sent [][]byte
	emit := func(msg []byte) error { sent = append(sent, msg); return nil }
	r := NewRequest(nil).
		WithMeta(map[string]any{"progressToken": "tok"}).
		WithEmitter(emit)

	if err := r.ReportProgress(ProgressUpdate{Progress: 1, Total: 4, Message: "step"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("want 1 frame, got %d", len(sent))
	}
	var n struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			ProgressToken string  `json:"progressToken"`
			Progress      float64 `json:"progress"`
			Total         float64 `json:"total"`
			Message       string  `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(sent[0], &n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n.JSONRPC != "2.0" || n.Method != "notifications/progress" {
		t.Fatalf("frame envelope = %+v", n)
	}
	if n.Params.ProgressToken != "tok" || n.Params.Progress != 1 || n.Params.Total != 4 || n.Params.Message != "step" {
		t.Fatalf("params = %+v", n.Params)
	}
}

func TestReportProgressOmitsUnsetTotalAndMessage(t *testing.T) {
	var sent [][]byte
	r := NewRequest(nil).
		WithMeta(map[string]any{"progressToken": float64(9)}).
		WithEmitter(func(msg []byte) error { sent = append(sent, msg); return nil })

	if err := r.ReportProgress(ProgressUpdate{Progress: 2}); err != nil {
		t.Fatalf("report: %v", err)
	}
	var params map[string]any
	var n struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(sent[0], &n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	params = n.Params
	if _, ok := params["total"]; ok {
		t.Fatalf("total should be omitted when <= 0: %v", params)
	}
	if _, ok := params["message"]; ok {
		t.Fatalf("message should be omitted when empty: %v", params)
	}
}

func TestReportProgressNoOpWithoutTokenOrEmitter(t *testing.T) {
	// No emitter: no-op, no error.
	r := NewRequest(nil).WithMeta(map[string]any{"progressToken": "t"})
	if err := r.ReportProgress(ProgressUpdate{Progress: 1}); err != nil {
		t.Fatalf("no-emitter report: %v", err)
	}

	// Emitter but no progressToken: no-op, nothing sent.
	var sent int
	r2 := NewRequest(nil).WithEmitter(func(msg []byte) error { sent++; return nil })
	if err := r2.ReportProgress(ProgressUpdate{Progress: 1}); err != nil {
		t.Fatalf("no-token report: %v", err)
	}
	if sent != 0 {
		t.Fatalf("expected no frames without a progressToken, got %d", sent)
	}
}
