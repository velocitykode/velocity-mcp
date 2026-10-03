package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
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

	read, err := readPages(ctx, c.proto.exchanged, listType, lim, hasLimit)
	if errors.Is(err, errAuthorizationChanged) {
		read, err = readPages(ctx, c.proto.exchanged, listType, lim, hasLimit)
	}
	if errors.Is(err, errAuthorizationChanged) {
		return unread(), newError("the credential presented to the server kept changing while the " +
			listType + " were being listed, so no listing of them belongs to one caller")
	}
	return read, err
}

// asker asks the server for one page of a listing and reports what it brought
// back. authorization is the context the page insists on being asked in, where
// zero insists on none.
type asker func(ctx context.Context, method string, params any, authorization int64) (answered, error)

// exchanged asks for a page in an exchange of its own.
func (p *protocol) exchanged(ctx context.Context, method string, params any, authorization int64) (answered, error) {
	return p.exchangeWith(ctx, method, params, terms{authorization: authorization})
}

// asked asks for a page over the held connection. The exchange is held, so the
// credential cannot change between two pages and no page is asked for in
// another context than the first.
func (h heldConnection) asked(ctx context.Context, method string, params any, _ int64) (answered, error) {
	return h.ask(ctx, method, params)
}

// maxListPages bounds how many pages a listing is followed for. A cursor is
// the server's to hand out, and a server that hands out a fresh one on every
// page would otherwise be followed, and its entries kept, for as long as the
// caller's context lasts: a tool call that reads the catalogue before it is
// sent has no bound of its own there. No catalogue a client could put to use
// runs anywhere near this many pages, so the bound costs a well-behaved server
// nothing and is reported to the caller when it is reached.
const maxListPages = 1000

// UnboundedListingError reports a listing this client stopped following: the
// server handed out a new cursor on every page past the number of pages a
// listing may run to. What was read is not returned, since a catalogue that was
// never read to its end is no catalogue at all.
type UnboundedListingError struct {
	// Method is the list request whose pages ran past the bound.
	Method string
	// Pages is the number of pages read before the listing was given up.
	Pages int
}

// Error implements the error interface.
func (e *UnboundedListingError) Error() string {
	if e == nil {
		return "<nil client unbounded listing error>"
	}
	return "the server kept handing out cursors for [" + e.Method + "] past " +
		strconv.Itoa(e.Pages) + " pages, so the listing was given up"
}

// readPages reads a listing from its first page to its last, or to the limit.
//
// The cursor is the server's and is opaque: the only thing read off it is
// whether the result carried one. A nextCursor member that is a string, the
// empty string included, names the page after this one and is sent back exactly
// as it arrived; the specification forbids treating the empty string as the end
// of the results. A member that is absent or null ends the listing, and one of
// any other type is a malformed result, reported rather than read as an end:
// a listing cut short would otherwise pass for the whole catalogue.
//
// Each page is asked for through ask, which is an exchange of its own for a
// listing a caller reads, and a request over a connection already held for one
// read on behalf of the call that holds it.
func readPages(ctx context.Context, ask asker, listType string, lim int, hasLimit bool) (listing, error) {
	read := unread()
	method := listType + "/list"
	var cursor json.RawMessage
	seen := map[string]bool{}
	pages := 0

	for {
		var params any
		if cursor != nil {
			if seen[string(cursor)] {
				return unread(), newError("repeated " + method + " cursor received from server")
			}
			seen[string(cursor)] = true
			params = map[string]any{"cursor": cursor}
		}
		if pages == maxListPages {
			return unread(), &UnboundedListingError{Method: method, Pages: pages}
		}

		reply, err := ask(ctx, method, params, read.authorization)
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

		var result map[string]json.RawMessage
		if err := json.Unmarshal(reply.result, &result); err != nil {
			return unread(), newError("invalid " + method + " response from server")
		}
		var page []any
		if err := json.Unmarshal(result[listType], &page); err != nil || page == nil {
			return unread(), newError("invalid " + method + " response from server")
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

		next, err := nextCursorOf(method, result)
		if err != nil {
			return unread(), err
		}
		if next == nil {
			return read, nil
		}
		cursor = next
	}
}

// nextCursorOf reads the cursor of the page after this one from a list result,
// returning nil when the result carries none: the member is absent or null,
// which is how a server says the listing has ended. A member of any type other
// than a string is reported, since the specification types the cursor as one.
//
// The cursor is returned as the member was written, not as the string it
// decodes to: it is opaque and goes back exactly as it arrived, and a spelling
// the decoder would replace or normalize, as a lone surrogate escape or an
// escaped letter, is lost the moment it is read into a string.
func nextCursorOf(method string, result map[string]json.RawMessage) (json.RawMessage, error) {
	stated := trimJSONSpace(result["nextCursor"])
	if len(stated) == 0 || bytes.Equal(stated, []byte("null")) {
		return nil, nil
	}
	if _, isString := jsonString(stated); !isString {
		return nil, newError("invalid " + method + " response from server: the nextCursor member is not a string")
	}
	return stated, nil
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
