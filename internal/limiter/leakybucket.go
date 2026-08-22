package limiter

import (
	"sync"
	"time"
)

// LeakyBucket is a mutex-guarded leaky-bucket rate limiter.
//
// The bucket is a virtual queue holding at most `capacity` units of water that
// drains ("leaks") continuously at `rate` units per second. Each admitted
// request pours its units in; a request is admitted only if the whole batch
// still fits under capacity. Draining is pure arithmetic over elapsed time (no
// background goroutine): every AllowN call first subtracts `elapsed * rate`
// from the water level, clamped at zero, then tries to pour.
//
// Compared with a token bucket the roles are inverted: the level starts at
// zero and admission *raises* it, so the steady-state admission rate is capped
// at the leak rate and bursts are smoothed into a constant outflow.
//
// LeakyBucket is safe for concurrent use by multiple goroutines.
//
// The zero value is not usable: it carries no Clock, so the first Allow call
// panics with a nil pointer dereference. Construct with NewLeakyBucket.
type LeakyBucket struct {
	mu sync.Mutex

	level    float64   // current water level; float64 because leaking drains fractional units
	rate     float64   // leak rate, units per second
	capacity float64   // maximum water level (queue size)
	last     time.Time // timestamp of the last leak recomputation

	clk Clock
}

// Compile-time assertion that LeakyBucket satisfies the Limiter contract.
var _ Limiter = (*LeakyBucket)(nil)

// NewLeakyBucket returns a LeakyBucket that drains at rate units per second
// with room for capacity units. The bucket starts empty, with the leak
// timestamp taken from clk.Now(). Pass SystemClock in production code; tests
// inject a fake Clock.
//
// Degenerate configurations are accepted rather than rejected — none of them
// can break the arithmetic (there is no division by a parameter, unlike
// NewSlidingWindow's window), they only make the limiter more restrictive:
//   - rate <= 0 means the bucket never drains, degenerating into a fixed
//     total budget of capacity units (a negative rate is clamped to "no leak"
//     in AllowN rather than letting the level rise spontaneously);
//   - capacity <= 0 leaves no room for any positive batch, so every
//     AllowN(n >= 1) is denied while AllowN(n <= 0) still returns true;
//   - a NaN rate behaves like "no leak" for the same reason a negative one
//     does (every comparison against NaN is false), and an infinite rate
//     empties the bucket on the first call that observes elapsed time.
func NewLeakyBucket(rate float64, capacity int, clk Clock) *LeakyBucket {
	return &LeakyBucket{
		rate:     rate,
		capacity: float64(capacity),
		last:     clk.Now(),
		clk:      clk,
	}
}

// Allow reports whether one request is permitted now.
func (b *LeakyBucket) Allow() bool { return b.AllowN(1) }

// AllowN attempts to admit n requests at once. The batch is all-or-nothing: if
// pouring n units would overflow capacity, nothing is poured and AllowN
// returns false. For n <= 0 it returns true without touching any state (not
// even the leak timestamp), per the Limiter port contract.
//
// The leak timestamp is advanced to clk.Now() unconditionally, which assumes
// the non-decreasing Clock the port requires (see Clock in limiter.go). A clock
// that runs backwards would rewind the timestamp and let the next call drain
// already-drained time twice; the guard below only keeps the level from rising
// on the spot. Detecting that is deliberately left out — it is the Clock
// implementation's contract to uphold, and TokenBucket makes the same
// assumption.
func (b *LeakyBucket) AllowN(n int) bool {
	if n <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clk.Now()
	elapsed := now.Sub(b.last)
	// A non-positive product (rate <= 0) drains nothing: the level must never
	// rise on its own, only when requests are admitted.
	if leaked := elapsed.Seconds() * b.rate; leaked > 0 {
		b.level = max(0, b.level-leaked)
	}
	b.last = now

	if b.level+float64(n) <= b.capacity+admitEpsilon {
		b.level += float64(n)
		return true
	}
	return false
}
