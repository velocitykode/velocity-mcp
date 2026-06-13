package server

import "testing"

func TestCompleteValuesPrefix(t *testing.T) {
	got := CompleteValues("la", []string{"Lahore", "London", "lapis", "Paris"})
	// Case-insensitive prefix match keeps Lahore and lapis.
	if len(got.Values) != 2 || got.Total != 2 || got.HasMore {
		t.Fatalf("completion = %+v", got)
	}
	if got.Values[0] != "Lahore" || got.Values[1] != "lapis" {
		t.Fatalf("values = %v", got.Values)
	}
}

func TestCompleteValuesEmptyValueMatchesAll(t *testing.T) {
	got := CompleteValues("", []string{"a", "b", "c"})
	if len(got.Values) != 3 || got.Total != 3 {
		t.Fatalf("empty value should match all, got %+v", got)
	}
}

func TestCompleteValuesCap(t *testing.T) {
	candidates := make([]string, MaxCompletionValues+5)
	for i := range candidates {
		candidates[i] = "x" // all match the "x" prefix
	}
	got := CompleteValues("x", candidates)
	if len(got.Values) != MaxCompletionValues {
		t.Fatalf("values not capped: %d", len(got.Values))
	}
	if got.Total != MaxCompletionValues+5 || !got.HasMore {
		t.Fatalf("expected Total=%d HasMore=true, got %+v", MaxCompletionValues+5, got)
	}
}

func TestCompleteValuesNoMatch(t *testing.T) {
	got := CompleteValues("z", []string{"a", "b"})
	// No match yields a non-nil empty slice (serializes as [], not null).
	if got.Values == nil || len(got.Values) != 0 || got.Total != 0 {
		t.Fatalf("expected empty non-nil, got %+v", got)
	}
}
