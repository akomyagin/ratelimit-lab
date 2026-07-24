package limiter

import (
	"sync"
	"time"
)

// fakeClock is a thread-safe controllable Clock for deterministic tests: time
// moves only when Advance is called, never by itself. It is shared by the
// correctness tests of every algorithm (token bucket, sliding window, leaky
// bucket, lock-free token bucket) and is read concurrently from stress tests,
// hence the mutex.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the fake current time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake time forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
