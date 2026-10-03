package transport

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/velocitykode/velocity/contract"
)

// TestBodyReadErrorKeepsTheSizeErrorAsItIs asserts the mapping itself: the size
// error travels unchanged, so the framework recognises it, and every other
// failure becomes the 400 with its cause attached.
func TestBodyReadErrorKeepsTheSizeErrorAsItIs(t *testing.T) {
	size := &http.MaxBytesError{Limit: 16}
	tests := []struct {
		name       string
		cause      error
		wantItself bool
	}{
		{"the size error", size, true},
		{"a truncated body", io.ErrUnexpectedEOF, false},
		{"any other failure", errors.New("connection reset"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bodyReadError(tt.cause)
			if tt.wantItself {
				if got != tt.cause {
					t.Fatalf("bodyReadError = %v, want the error itself", got)
				}
				return
			}
			var httpErr *contract.HTTPError
			if !errors.As(got, &httpErr) || httpErr.Status != http.StatusBadRequest || !errors.Is(got, tt.cause) {
				t.Fatalf("bodyReadError = %v, want a 400 wrapping the cause", got)
			}
		})
	}
}
