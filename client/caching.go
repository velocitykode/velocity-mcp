package client

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// This file reads the caching hints of the protocol revision that defines them.
// A server states on the results of server/discover and of the list operations
// how long a client may consider them fresh, in the "ttlMs" member. This client
// keeps two such results past the call that read them: the discover result a
// connection was settled with, and the mirrored parameters of the tool
// definitions a listing stated. Both are kept only for as long as the server
// said they may be, and read again the next time they are needed after that.
//
// The "cacheScope" member is not read. It says whether a result may be shared
// across authorization contexts, and nothing here ever is: what the client
// keeps is kept for the one credential it was fetched with (see
// AuthorizationAware), which is the narrower of the two scopes the
// specification defines and the one it permits for every result.

// lifetimeOf reads how long a result may be considered fresh from its "ttlMs"
// member, which states it in whole milliseconds.
//
// The specification has a client read an absent hint as zero and ignore a
// negative one as zero, and zero is the lifetime of a result that is stale the
// moment it arrives. A hint that is not a whole number of milliseconds (a
// string, a fraction, a number beyond what an integer holds) is one this client
// cannot date a result with, and is read the same way: a result that is not
// kept costs a request, and never stands in for what the server has since
// changed. A number written in another form JSON allows for the same value
// (3e5 or 300000.0) is read as the whole number it is.
func lifetimeOf(result json.RawMessage) time.Duration {
	var hints struct {
		TTL json.RawMessage `json:"ttlMs"`
	}
	if err := json.Unmarshal(result, &hints); err != nil {
		return 0
	}
	text := string(bytes.TrimSpace(hints.TTL))
	milliseconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		whole, isWhole := integralNumber(text)
		if !isWhole {
			return 0
		}
		milliseconds = whole
	}
	if milliseconds <= 0 {
		return 0
	}
	// A lifetime longer than a duration holds is held to the longest one it
	// does, rather than wrapping round into one that has already run out or
	// into one the server never stated.
	if milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return math.MaxInt64
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// staleAfter returns the moment a result received at receivedAt stops being
// fresh. The specification's rule is that a result is fresh while the local
// time is before the time it was received plus its lifetime, so it is stale
// from this moment on, the moment itself included.
func staleAfter(receivedAt time.Time, result json.RawMessage) time.Time {
	return receivedAt.Add(lifetimeOf(result))
}
