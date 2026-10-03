package client

import "testing"

// TestTailBufferKeepsTheLastBytes pins the buffer itself: it never holds more
// than its limit, what it holds is the end of what was written, and it reports
// every write as whole.
func TestTailBufferKeepsTheLastBytes(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{"under the limit everything is kept", 8, []string{"abc", "de"}, "abcde"},
		{"exactly the limit is kept whole", 4, []string{"ab", "cd"}, "abcd"},
		{"the oldest bytes go first", 4, []string{"abc", "def"}, "cdef"},
		{"one write larger than the limit keeps its tail", 4, []string{"x", "abcdefgh"}, "efgh"},
		{"a write of exactly the limit replaces what was kept", 4, []string{"zz", "abcd"}, "abcd"},
		{"many small writes", 3, []string{"a", "b", "c", "d", "e"}, "cde"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buffer := &tailBuffer{limit: tt.limit}
			for _, write := range tt.writes {
				n, err := buffer.Write([]byte(write))
				if err != nil || n != len(write) {
					t.Fatalf("Write(%q) = %d, %v; want it reported whole", write, n, err)
				}
			}
			if got := buffer.String(); got != tt.want {
				t.Fatalf("kept %q, want %q", got, tt.want)
			}
		})
	}
}
