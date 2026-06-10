package mcptest

import "testing"

func TestAsMaps(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
	}{
		{"nil", nil, 0},
		{"not array", map[string]any{"a": 1}, 0},
		{"mixed elements", []any{map[string]any{"x": 1}, 42, "str"}, 1},
		{"all maps", []any{map[string]any{}, map[string]any{}}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := asMaps(tt.in); len(got) != tt.want {
				t.Fatalf("asMaps(%v) len = %d, want %d", tt.in, len(got), tt.want)
			}
		})
	}
}

func TestFirstString(t *testing.T) {
	m := map[string]any{"text": "hello", "n": 3}
	if got := firstString(m, "data", "text"); got != "hello" {
		t.Fatalf("firstString = %q, want hello", got)
	}
	if got := firstString(m, "n"); got != "" {
		t.Fatalf("firstString on non-string = %q, want empty", got)
	}
	if got := firstString(m, "absent"); got != "" {
		t.Fatalf("firstString on absent = %q, want empty", got)
	}
}

func TestContainsAny(t *testing.T) {
	in := []string{"alpha", "beta"}
	if !containsAny(in, "lph") {
		t.Fatal("containsAny should match substring")
	}
	if containsAny(in, "zzz") {
		t.Fatal("containsAny should not match missing")
	}
	if containsAny(nil, "x") {
		t.Fatal("containsAny on nil should be false")
	}
}

func TestDedupeNonEmpty(t *testing.T) {
	in := []string{"a", "", "b", "a", "", "c", "b"}
	got := dedupeNonEmpty(in)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupeNonEmpty = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeNonEmpty[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := dedupeNonEmpty(nil); len(got) != 0 {
		t.Fatalf("dedupeNonEmpty(nil) = %v, want empty", got)
	}
}

func TestJSONEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"int vs float", 5, 5.0, true},
		{"string match", "x", "x", true},
		{"map match", map[string]any{"k": 1}, map[string]any{"k": 1.0}, true},
		{"mismatch", 1, 2, false},
		{"string vs number", "1", 1, false},
		{"unmarshalable a", make(chan int), 1, false},
		{"unmarshalable b", 1, make(chan int), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonEqual(tt.a, tt.b); got != tt.want {
				t.Fatalf("jsonEqual(%v,%v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDecodeResponse_Malformed(t *testing.T) {
	if _, err := decodeResponse([]byte("{not json")); err == nil {
		t.Fatal("decodeResponse should error on malformed JSON")
	}
	resp, err := decodeResponse(nil)
	if err != nil || resp != nil {
		t.Fatalf("decodeResponse(nil) = %v, %v; want nil,nil", resp, err)
	}
}
