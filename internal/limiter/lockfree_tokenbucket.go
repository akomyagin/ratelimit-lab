package limiter

import (
	"sync/atomic"
	"time"
)

// lockFreeState is one immutable snapshot of the bucket state. AllowN never
// mutates a published snapshot: it computes a fresh one and installs it with a
// single CompareAndSwap, so readers always observe a consistent (tokens, last)
// pair.
type lockFreeState struct {
	tokens float64   // current token level; float64 because refill accrues fractional tokens
	last   time.Time // timestamp of the last committed refill recomputation
}

// LockFreeTokenBucket is a token-bucket rate limiter that coordinates through
// a CAS loop over sync/atomic instead of a mutex.
//
// The token math is identical to TokenBucket (the mutex baseline): tokens
// refill continuously at `rate` per second up to `capacity`, each allowed
// request consumes one token, and refill is pure arithmetic over elapsed time
// (no background ticker goroutine). Only the coordination differs — state
// transitions are published with atomic.Pointer.CompareAndSwap, so concurrent
// callers retry against fresh state instead of queueing on a lock.
//
// State packing: the (tokens, last) pair is held behind an
// atomic.Pointer[lockFreeState] pointing at an immutable snapshot, rather than
// bit-packed into a single uint64. The pointer form keeps tokens a plain
// float64 and last a full time.Time, so the refill is the same expression at
// the same precision as the mutex baseline — not the same order of operations,
// because the deny-path divergence documented on AllowN makes this limiter
// credit one large elapsed where the baseline credits many small ones, so the
// two can differ in the low bits. It also rules out ABA structurally, because
// a snapshot's address cannot be reused while any CAS loop still holds the old
// pointer (the GC keeps it alive). The cost is one
// small allocation per loop iteration, which shows up as GC pressure under
// contention in the Этап 5 benchmarks — accepted here, since a uint64 packing
// would force fixed-point tokens and a truncated timestamp, i.e. different
// arithmetic than the baseline this limiter must match observably.
//
// LockFreeTokenBucket is safe for concurrent use by multiple goroutines.
//
// The zero value is not usable: it carries no Clock and no initial state
// snapshot, so the first Allow call panics with a nil pointer dereference.
// Construct with NewLockFreeTokenBucket.
type LockFreeTokenBucket struct {
	state atomic.Pointer[lockFreeState]

	rate     float64 // refill rate, tokens per second
	capacity float64 // maximum token level

	clk Clock
}

// Compile-time assertion that LockFreeTokenBucket satisfies the Limiter contract.
var _ Limiter = (*LockFreeTokenBucket)(nil)

// NewLockFreeTokenBucket returns a LockFreeTokenBucket that refills at rate
// tokens per second up to capacity. The bucket starts full, with the refill
// timestamp taken from clk.Now(). Pass SystemClock in production code; tests
// inject a fake Clock.
func NewLockFreeTokenBucket(rate float64, capacity int, clk Clock) *LockFreeTokenBucket {
	b := &LockFreeTokenBucket{
		rate:     rate,
		capacity: float64(capacity),
		clk:      clk,
	}
	b.state.Store(&lockFreeState{
		tokens: float64(capacity),
		last:   clk.Now(),
	})
	return b
}

// Allow reports whether one request is permitted now.
func (b *LockFreeTokenBucket) Allow() bool { return b.AllowN(1) }

// AllowN attempts to consume n tokens at once. The batch is all-or-nothing: if
// fewer than n tokens are available, nothing is consumed and AllowN returns
// false. For n <= 0 it returns true without touching any state (not even the
// refill timestamp), per the Limiter port contract.
//
// Deliberate divergence from the mutex baseline, do not "fix": a denied call
// returns false without a CAS, so the state — including the refill timestamp —
// is not updated on denial, whereas TokenBucket persists the refill even when
// it denies. This is observably equivalent up to the admitEpsilon boundary:
// the next call recomputes the refill from the older timestamp over a
// correspondingly larger elapsed, and the accrued credit is the same real
// number (min(cap, x+a+b) == min(cap, min(cap, x+a)+b) for a, b >= 0). Only a
// configuration that sits exactly on the epsilon threshold can round the two
// orders of operations to opposite verdicts, and then systematically — e.g.
// capacity 1, rate 0.5, AllowN(1) every 333333333ns admits ~7% fewer requests
// here than in the baseline. Forcing a CAS on the deny path would make denied
// callers spin against admitted ones for an update they do not need.
//
// Why the CAS-free deny is sound under concurrency: the snapshot it decided on
// may already be stale, and no CAS on this path would notice. It is still
// correct because recomputing from an older last over a larger elapsed yields
// an upper bound on the true current token level — clamping is associative in
// the sense above, and every commit this call missed additionally subtracted
// its own n. A denial computed on an over-estimate therefore implies a denial
// on the true state, so a false admission is impossible. The reverse, a
// denial that a fresher snapshot would have allowed, is only possible "in
// time" — the call linearizes at its own b.clk.Now(), which is legitimate.
//
// Backoff on a failed CAS: none — the loop retries immediately. A failed CAS
// means another goroutine just committed its own transition, so the system as
// a whole made progress (the lock-free guarantee); the retry costs one load
// and one pure recomputation. Yielding (runtime.Gosched) or spinning with
// pauses would trade that simplicity for scheduler latency without a
// correctness benefit, and the project favors readable coordination code over
// benchmark-tuned backoff (SKILL §6).
func (b *LockFreeTokenBucket) AllowN(n int) bool {
	if n <= 0 {
		return true
	}

	for {
		old := b.state.Load()
		// Read the clock after loading the state: old.last came from a Now()
		// call that completed before the snapshot was published, so a Now()
		// issued after the load is >= old.last under the Clock contract and
		// elapsed below is never negative.
		now := b.clk.Now()
		next, ok := nextLockFreeState(old, now, b.rate, b.capacity, n)
		if !ok {
			return false // no tokens for the batch — deny without touching state
		}
		if b.state.CompareAndSwap(old, next) {
			return true
		}
		// CAS lost to a concurrent update — retry against the fresh state.
	}
}

// nextLockFreeState is the pure transition function of the CAS loop: given the
// old snapshot and the current time, it returns the refilled-and-spent next
// snapshot and whether the batch of n tokens was admitted. It has no side
// effects and touches no shared state — all nondeterminism lives in the loop —
// which is what makes the stress test a meaningful check of the coordination
// rather than of the arithmetic.
//
// The spend comparison carries admitEpsilon because `tokens` accrues as a
// float64 across many partial refills: without it, a client calling at exactly
// the configured rate is denied on every boundary request (see admitEpsilon in
// limiter.go).
func nextLockFreeState(old *lockFreeState, now time.Time, rate, capacity float64, n int) (*lockFreeState, bool) {
	tokens := min(capacity, old.tokens+now.Sub(old.last).Seconds()*rate)
	if tokens+admitEpsilon >= float64(n) {
		return &lockFreeState{tokens: tokens - float64(n), last: now}, true
	}
	return nil, false
}
