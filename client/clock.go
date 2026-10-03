package client

import (
	"context"
	"sync"
	"time"
)

// exchangeClock is the context one exchange runs under when the caller gave it
// no deadline. It carries the two timeouts the MCP specification describes for
// a request (basic/lifecycle#timeouts). The first is the timeout itself, which
// a progress notification for the request starts over: a server that reports
// progress is working, and the request is not cut while it does. The second is
// the maximum, enforced whatever progress is reported, so no server can hold a
// caller for ever by reporting it: that one is the deadline of the parent
// context.
//
// It ends as a context whose deadline passed in either case, which is what a
// transport reading under it reports as a timeout.
type exchangeClock struct {
	context.Context

	idle time.Duration
	done chan struct{}

	mu       sync.Mutex
	err      error
	timer    *time.Timer
	detached func() bool
}

// newExchangeClock starts a clock under parent, whose deadline is the maximum,
// that ends after idle unless progress starts it over.
func newExchangeClock(parent context.Context, idle time.Duration) *exchangeClock {
	clock := &exchangeClock{Context: parent, idle: idle, done: make(chan struct{})}
	// Either callback may run before this function returns, and both read what
	// is being set here, so they are started with the clock held.
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.timer = time.AfterFunc(idle, func() { clock.end(context.DeadlineExceeded) })
	clock.detached = context.AfterFunc(parent, func() { clock.end(parent.Err()) })
	return clock
}

// Done implements context.Context.
func (c *exchangeClock) Done() <-chan struct{} { return c.done }

// Err implements context.Context.
func (c *exchangeClock) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// end ends the clock with the given error, once.
func (c *exchangeClock) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	c.timer.Stop()
	close(c.done)
}

// progressed starts the timeout over. It does nothing once the clock has ended:
// progress reported for a request that already timed out does not revive it.
func (c *exchangeClock) progressed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.timer.Reset(c.idle)
	}
}

// release ends the clock when the exchange is over and lets go of what it
// holds.
func (c *exchangeClock) release() {
	c.end(context.Canceled)
	c.detached()
}
