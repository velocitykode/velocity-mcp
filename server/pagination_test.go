package server

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestCursorPaginator(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}

	tests := []struct {
		name       string
		perPage    int
		cursor     string
		wantPage   []string
		wantHasNxt bool
	}{
		{"first page", 2, "", []string{"a", "b"}, true},
		{"full coverage single page", 10, "", []string{"a", "b", "c", "d", "e"}, false},
		{"exact page no next", 5, "", []string{"a", "b", "c", "d", "e"}, false},
		{"zero per page", 0, "", []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, next := NewCursorPaginator(items, tt.perPage, tt.cursor).Paginate()
			if len(page) != len(tt.wantPage) {
				t.Fatalf("page len = %d want %d", len(page), len(tt.wantPage))
			}
			for i := range page {
				if page[i] != tt.wantPage[i] {
					t.Fatalf("page[%d] = %q want %q", i, page[i], tt.wantPage[i])
				}
			}
			if (next != "") != tt.wantHasNxt {
				t.Fatalf("hasNext = %v want %v", next != "", tt.wantHasNxt)
			}
		})
	}
}

func TestCursorPaginatorRoundTrip(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6}
	var collected []int
	cursor := ""
	for {
		page, next := NewCursorPaginator(items, 3, cursor).Paginate()
		collected = append(collected, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(collected) != len(items) {
		t.Fatalf("collected %d items, want %d", len(collected), len(items))
	}
	for i := range items {
		if collected[i] != items[i] {
			t.Fatalf("collected[%d] = %d want %d", i, collected[i], items[i])
		}
	}
}

func TestCursorPaginator_EdgeCase_MalformedCursor(t *testing.T) {
	items := []string{"a", "b", "c"}

	// A non-base64 cursor, a non-JSON cursor, and a negative offset all fall
	// back to offset 0 (first page), mirroring laravel's defensive decode.
	bad := []string{
		"!!!not-base64!!!",
		base64.StdEncoding.EncodeToString([]byte("not json")),
		encodeCursor(-5),
	}
	for _, c := range bad {
		page, _ := NewCursorPaginator(items, 2, c).Paginate()
		if len(page) != 2 || page[0] != "a" {
			t.Fatalf("malformed cursor %q did not reset to first page: %v", c, page)
		}
	}
}

func TestEncodeCursor(t *testing.T) {
	c := encodeCursor(4)
	decoded, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		t.Fatalf("cursor not base64: %v", err)
	}
	var data struct {
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(decoded, &data); err != nil {
		t.Fatalf("cursor not json: %v", err)
	}
	if data.Offset != 4 {
		t.Fatalf("offset = %d", data.Offset)
	}
}

func TestCursorOffsetBeyondLen(t *testing.T) {
	items := []string{"a", "b"}
	page, next := NewCursorPaginator(items, 2, encodeCursor(10)).Paginate()
	if len(page) != 0 || next != "" {
		t.Fatalf("offset beyond length should yield empty final page, got %v next=%q", page, next)
	}
}
