package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// list fetches every entry of a paginated list method (tools/list, etc.),
// transparently following nextCursor. listType is the plural primitive name and
// also the result key holding the page. A non-empty limit caps the number of
// entries returned (a limit of zero returns none).
func (c *Client) list(ctx context.Context, listType string, limit []int) ([]map[string]any, error) {
	read, err := c.listOn(ctx, listType, limit)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(read.entries))
	for _, entry := range read.entries {
		items = append(items, entry.payload)
	}
	return items, nil
}

// listing is what a paginated read brought back: its entries, the connection
// its pages were read over, the moment the first of those pages to go stale
// does, and the authorization context every one of them was asked for in. What
// a listing states as a whole (that the server advertises nothing under a name
// it does not carry) rests on every page of it, so it stands only for as long
// as all of them do.
type listing struct {
	entries       []listedEntry
	readOn        int64
	staleAfter    time.Time
	authorization int64
}

// listedEntry is one entry of a listing and the moment the page that carried it
// stops being fresh. The specification dates every page on its own: each states
// its own lifetime, and the clock of each starts when that page arrived.
type listedEntry struct {
	payload    map[string]any
	staleAfter time.Time
}

// unread is the listing of a read that brought nothing back.
func unread() listing { return listing{readOn: crossedGeneration} }

// listOn is list, also reporting the connection the pages were read over and
// how long what they state may be kept.
//
// The connection of a page is the one its exchange reports, read while that
// exchange was still held: it is the connection the page travelled over and no
// other. Asking the protocol for the connection once the page is in hand would
// not do. The exchange has let go by then, a handshake waiting behind it may
// settle before the question is put, and the page would be stamped with a
// connection it never travelled over: a listing of one page would then pass for
// what the server reached again states, and settle the headers of calls it says
// nothing about. The time a page arrived is reported by its exchange the same
// way, and the lifetime the page states is counted from it.
//
// A listing whose pages did not all travel over the same connection read them
// from more than one server, so it reports crossedGeneration: its pages are one
// catalogue of nobody, and nothing later may take them for what the server now
// standing states. A listing of no pages at all reports the same, having read
// nothing from anyone.
//
// Every page after the first is asked for in the authorization context the
// first was. A cursor is something the server handed to one caller, and the
// pages read with it are its answers to that caller: were the credential to
// change between two pages, the cursor would be presented by someone it was not
// handed to, and the pages of two callers returned as the catalogue of one. The
// page is refused before it is sent instead, what was read is dropped, and the
// listing is read once more from the start under the credential now presented.
// A credential that changes again in the middle of that is reported rather than
// chased.
func (c *Client) listOn(ctx context.Context, listType string, limit []int) (listing, error) {
	lim, hasLimit, err := resolveLimit(listType, limit)
	if err != nil {
		return unread(), err
	}
	if hasLimit && lim == 0 {
		return unread(), nil
	}

	read, err := c.readPages(ctx, listType, lim, hasLimit)
	if errors.Is(err, errAuthorizationChanged) {
		read, err = c.readPages(ctx, listType, lim, hasLimit)
	}
	if errors.Is(err, errAuthorizationChanged) {
		return unread(), newError("the credential presented to the server kept changing while the " +
			listType + " were being listed, so no listing of them belongs to one caller")
	}
	return read, err
}

// readPages reads a listing from its first page to its last, or to the limit.
func (c *Client) readPages(ctx context.Context, listType string, lim int, hasLimit bool) (listing, error) {
	read := unread()
	cursor := ""
	seen := map[string]bool{}
	pages := 0

	for {
		if cursor != "" {
			if seen[cursor] {
				return unread(), newError("repeated " + listType + "/list cursor [" + cursor + "] received from server")
			}
			seen[cursor] = true
		}

		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}

		reply, err := c.proto.exchangeWith(ctx, listType+"/list", params, terms{authorization: read.authorization})
		if err != nil {
			return unread(), err
		}
		pageStaleAfter := staleAfter(reply.receivedAt, reply.result)
		if pages == 0 {
			read.readOn = reply.generation
			read.staleAfter = pageStaleAfter
			read.authorization = reply.authorization
		} else {
			if reply.generation != read.readOn {
				read.readOn = crossedGeneration
			}
			if pageStaleAfter.Before(read.staleAfter) {
				read.staleAfter = pageStaleAfter
			}
		}
		pages++

		var result map[string]any
		if err := json.Unmarshal(reply.result, &result); err != nil {
			return unread(), newError("invalid " + listType + "/list response from server")
		}
		page, ok := result[listType].([]any)
		if !ok {
			return unread(), newError("invalid " + listType + "/list response from server")
		}
		for _, entry := range page {
			m, ok := entry.(map[string]any)
			if !ok {
				return unread(), newError("invalid " + listType + " payload from server")
			}
			if hasLimit && len(read.entries) >= lim {
				return read, nil
			}
			read.entries = append(read.entries, listedEntry{payload: m, staleAfter: pageStaleAfter})
		}

		next, _ := result["nextCursor"].(string)
		if next == "" {
			return read, nil
		}
		cursor = next
	}
}

// resolveLimit interprets the optional variadic limit: absent means unlimited, a
// negative value is an error, and a non-negative value caps the result count.
func resolveLimit(listType string, limit []int) (int, bool, error) {
	if len(limit) == 0 {
		return 0, false, nil
	}
	if limit[0] < 0 {
		return 0, false, newError(listType + " list limit must be greater than or equal to zero")
	}
	return limit[0], true, nil
}
