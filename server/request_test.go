package server

import (
	"encoding/json"
	"errors"
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
		"name":  {"required"},
		"email": {"required", "email"},
	})
	if err != nil {
		t.Fatalf("validate passed should be nil: %v", err)
	}
}

func TestRequestValidate_EdgeCase(t *testing.T) {
	req := NewRequest(map[string]any{"name": ""})
	err := req.Validate(validation.Rules{"name": {"required"}})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error should wrap ErrValidation: %v", err)
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
