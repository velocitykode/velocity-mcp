package server

import (
	"encoding/base64"
	"encoding/json"
)

// CursorPaginator slices an ordered item set into cursor-delimited pages,
// mirroring laravel/mcp's Server\Pagination\CursorPaginator. The cursor is an
// opaque base64-encoded JSON object carrying the next page's start offset; an
// unreadable or malformed cursor is treated as the start of the list (offset 0),
// matching laravel's defensive decode.
type CursorPaginator[T any] struct {
	items   []T
	perPage int
	cursor  string
}

// NewCursorPaginator builds a paginator over items with the given page size and
// incoming cursor (empty for the first page). A perPage of zero or less yields a
// single empty page; callers normally pass a positive page size resolved by
// Context.PerPage.
func NewCursorPaginator[T any](items []T, perPage int, cursor string) *CursorPaginator[T] {
	return &CursorPaginator[T]{items: items, perPage: perPage, cursor: cursor}
}

// Paginate returns the page slice and the next cursor. nextCursor is empty when
// there are no further pages. The page is always a non-nil slice (possibly
// empty) so JSON encoders emit "[]" rather than "null", matching the MCP wire
// shape laravel produces.
func (p *CursorPaginator[T]) Paginate() (page []T, nextCursor string) {
	start := p.startOffset()
	page = []T{}

	if start < 0 {
		start = 0
	}
	if p.perPage <= 0 || start >= len(p.items) {
		return page, ""
	}

	end := start + p.perPage
	if end > len(p.items) {
		end = len(p.items)
	}
	page = append(page, p.items[start:end]...)

	if len(p.items) > start+p.perPage {
		nextCursor = encodeCursor(start + p.perPage)
	}
	return page, nextCursor
}

// startOffset decodes the incoming cursor to a start offset, defaulting to 0 on
// any decode failure, mirroring laravel's getStartOffsetFromCursor.
func (p *CursorPaginator[T]) startOffset() int {
	if p.cursor == "" {
		return 0
	}
	decoded, err := base64.StdEncoding.DecodeString(p.cursor)
	if err != nil {
		return 0
	}
	var data struct {
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return 0
	}
	if data.Offset < 0 {
		return 0
	}
	return data.Offset
}

// encodeCursor encodes an offset into an opaque cursor, mirroring laravel's
// createCursor (base64 of {"offset":N}).
func encodeCursor(offset int) string {
	b, err := json.Marshal(struct {
		Offset int `json:"offset"`
	}{Offset: offset})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}
