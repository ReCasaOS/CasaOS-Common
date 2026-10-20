package stats

import (
	"sync"
	"time"
)

// Clock is the time source; tests inject a FakeClock.
type Clock interface{ Now() time.Time }

// SystemClock reads the real time.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// FakeClock is a manually driven Clock, safe for concurrent use.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

var _ Clock = (*FakeClock)(nil)

func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
