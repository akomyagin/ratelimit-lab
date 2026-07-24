package limiter

import (
	"sync"
	"time"
)

// TokenBucket is a mutex-guarded token-bucket rate limiter.
//
// Tokens refill continuously at `rate` per second up to `capacity`; each allowed
// request consumes one token. Refill is pure arithmetic over elapsed time
// (no background ticker goroutine): every AllowN call first credits
// `elapsed * rate` tokens, clamped to capacity, then tries to spend.
//
// This is the straightforward, obviously-correct baseline implementation — the
// lock-free CAS variant added in Этап 4 (LockFreeTokenBucket) is benchmarked
// against it to show what lock-free buys and what it costs in complexity.
//
// TokenBucket is safe for concurrent use by multiple goroutines.
type TokenBucket struct {
	mu sync.Mutex

	tokens   float64   // current token level; float64 because refill accrues fractional tokens
	rate     float64   // refill rate, tokens per second
	capacity float64   // maximum token level
	last     time.Time // timestamp of the last refill recomputation

	clk Clock
}

// Compile-time assertion that TokenBucket satisfies the Limiter contract.
var _ Limiter = (*TokenBucket)(nil)

// NewTokenBucket returns a TokenBucket that refills at rate tokens per second
// up to capacity. The bucket starts full, with the refill timestamp taken from
// clk.Now(). Pass SystemClock in production code; tests inject a fake Clock.
func NewTokenBucket(rate float64, capacity int, clk Clock) *TokenBucket {
	return &TokenBucket{
		tokens:   float64(capacity),
		rate:     rate,
		capacity: float64(capacity),
		last:     clk.Now(),
		clk:      clk,
	}
}

// Allow reports whether one request is permitted now.
func (b *TokenBucket) Allow() bool { return b.AllowN(1) }

// AllowN attempts to consume n tokens at once. The batch is all-or-nothing: if
// fewer than n tokens are available, nothing is consumed and AllowN returns
// false. For n <= 0 it returns true without touching any state (not even the
// refill timestamp), per the Limiter port contract.
func (b *TokenBucket) AllowN(n int) bool {
	if n <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clk.Now()
	elapsed := now.Sub(b.last)
	b.tokens = min(b.capacity, b.tokens+elapsed.Seconds()*b.rate)
	b.last = now

	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true
	}
	return false
}
