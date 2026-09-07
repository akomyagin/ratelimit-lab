package limiter

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// maxBucketSpanNanos bounds both the emission interval and the full-bucket span
// an AtomicTokenBucket may be configured with. At 1<<62 ns (~146 years) it is
// large enough that no honest configuration reaches it, and small enough that
// every intermediate value in the hot path provably stays inside int64 — see
// the overflow argument on nextAtomicState.
const maxBucketSpanNanos = 1 << 62

// AtomicTokenBucket is a token-bucket rate limiter whose entire mutable state
// is one atomic int64, laid out so that the CAS loop touches exactly one cache
// line.
//
// The enforced policy is the same as TokenBucket's: the bucket starts full,
// credit accrues continuously at `rate` per second up to `capacity`, and an
// admitted request of n units consumes n. What differs is the representation
// and, deliberately, nothing else observable at the port — the divergences are
// enumerated on AllowN.
//
// # Time instead of tokens
//
// Nothing counts tokens here. The single word vt holds a "virtual zero time":
// the instant, in nanoseconds since the bucket's origin, at which the credit
// would be exactly zero. Credit is derived, not stored:
//
//	tokens(now) = min(capacity, (nowNs - vt) / T),  T = round(1e9 / rate)
//
// so a full bucket at construction is simply vt = -capacity*T. Admitting n
// units moves vt forward by n*T; idle time moves now forward and the gap widens
// on its own. This is the dual of the token counter, and it is what Envoy's
// AtomicTokenBucketImpl and GCRA-family limiters store (RESEARCH §2.1).
//
// Two things follow, and both are the point of the representation. First, the
// state fits in one word, so a transition is one CompareAndSwap with no
// allocation — LockFreeTokenBucket needs an allocated snapshot per attempt
// because it must publish a (tokens, last) pair atomically. Second, all hot-path
// arithmetic is exact int64: nothing rounds, so there is no float64 residue to
// accumulate and admitEpsilon plays no part here (see AllowN).
//
// # Memory layout is part of the design, not tidiness
//
// The 120 bytes of padding below are load-bearing and measured; do not
// "tidy the fields". A failed CompareAndSwap is a full memory barrier, so any
// struct field the loop reads must be re-loaded from memory on every retry —
// and if it shares a cache line with the CAS target, that line is exactly the
// one concurrent writers keep invalidating. Separating the read-only fields
// from vt by 128 bytes guarantees at least one whole untouched line between
// them whatever the heap alignment happens to be. 64 is not enough: the
// adjacent-line prefetcher works in pairs, so invalidating vt's line still
// reaches line+1 — measured worse than no padding at all (RESEARCH §1.3).
// sync/pool.go pads to the same 128 for the same reason.
//
// The complementary half of that argument lives in AllowN: the padding only
// helps fields the loop still reads, and the loop is written so that it reads
// none of them.
//
// AtomicTokenBucket is safe for concurrent use by multiple goroutines.
//
// The zero value is not usable: it carries no calibrated emission interval and
// no capacity, so every call silently denies — capacity == 0 makes AllowN(1)
// return false at the early `int64(n) > b.capacity` check, never reaching clk.
// There is no nil-Clock panic to warn about; the failure mode is silent
// rejection, not a crash. Construct with NewAtomicTokenBucket.
type AtomicTokenBucket struct {
	// vt is the only mutable word and the sole CAS target: the instant, in
	// nanoseconds since origin, at which the credit is zero.
	vt atomic.Int64

	// Padding to push every read-only field two cache lines clear of vt. See
	// the type comment — this is a measured part of the design.
	_ [120]byte

	nanosPerToken int64      // T = round(1e9/rate): emission interval, ns per unit
	capT          int64      // capacity*T: the full-bucket span in ns (0 if capacity <= 0)
	capacity      int64      // capacity as given: the early-deny bound for AllowN
	origin        time.Time  // reference instant nowNs is measured from
	clk           Clock      // fallback time source
	sinceClk      SinceClock // non-nil iff clk offers the monotonic fast path
}

// Compile-time assertion that AtomicTokenBucket satisfies the Limiter contract.
var _ Limiter = (*AtomicTokenBucket)(nil)

// NewAtomicTokenBucket returns an AtomicTokenBucket that refills at rate units
// per second up to capacity. The bucket starts full, with its time origin taken
// from clk.Now(). Pass SystemClock in production code; tests inject a fake
// Clock.
//
// If clk also implements SinceClock, the hot path uses it and reads only the
// monotonic clock; the assertion is done here, once, never per call.
//
// # Rate is quantized, and that is documented rather than fixed
//
// The rate is stored as an integer emission interval T = round(1e9/rate) ns, so
// the rate actually enforced is 1e9/T, not rate. The relative calibration error
// is at most rate/2e9 and it does not accumulate: it is a fixed error in the
// period, not a drift, exactly as in Rust's governor and uber-go's rate limiter.
// At rate <= 2000 the error is under 1e-6; at rate = 1e6 it is under 5e-4; at
// rate = 6e8 it reaches 20%, because 1.67 ns rounds to 2. The benchmark default
// of 1e9 gives T = 1 exactly. Callers needing float-exact rates at hundreds of
// millions per second want the float64 baseline, not this limiter.
//
// # Why this constructor panics where TokenBucket accepts anything
//
// Project rule: a constructor validates precisely the parameters its arithmetic
// divides by (as NewSlidingWindow does). This one computes 1e9/rate, so a rate
// that makes that meaningless is a programming error, reported immediately
// rather than turned into a silently dead limiter. It panics if rate is NaN or
// non-positive; if round(1e9/rate) < 1, i.e. rate > 2e9, since an emission
// interval below one nanosecond is not representable (+Inf lands here too); if
// the interval exceeds maxBucketSpanNanos; and if capacity*T would exceed it,
// which is the overflow trap that bites real configurations — a rate of one per
// hour with a burst of three million already overflows the product (RESEARCH §2).
//
// A non-positive capacity is accepted, consistently with the other limiters:
// degenerate capacity only tightens the limit, and the resulting bucket simply
// denies every request of one or more units.
func NewAtomicTokenBucket(rate float64, capacity int, clk Clock) *AtomicTokenBucket {
	// NaN is checked explicitly because every comparison against it is false,
	// so `rate <= 0` alone would let it through.
	if math.IsNaN(rate) || rate <= 0 {
		panic(fmt.Sprintf("limiter: NewAtomicTokenBucket: rate must be a positive number, got %g", rate))
	}

	// Validate the period as a float64, before the conversion: converting an
	// out-of-range float to int64 is unspecified in Go.
	period := math.Round(1e9 / rate)
	if period < 1 {
		panic(fmt.Sprintf("limiter: NewAtomicTokenBucket: rate %g needs an emission interval of %g ns, below the 1 ns resolution of this limiter (rate must be <= 2e9)", rate, 1e9/rate))
	}
	if period > maxBucketSpanNanos {
		panic(fmt.Sprintf("limiter: NewAtomicTokenBucket: rate %g needs an emission interval of %g ns, beyond the %d ns limit of this limiter", rate, period, int64(maxBucketSpanNanos)))
	}
	nanosPerToken := int64(period)

	var capT int64
	if capacity > 0 {
		if int64(capacity) > maxBucketSpanNanos/nanosPerToken {
			panic(fmt.Sprintf("limiter: NewAtomicTokenBucket: capacity %d at rate %g spans %d x %d ns, overflowing the %d ns limit of this limiter; lower the capacity or raise the rate", capacity, rate, capacity, nanosPerToken, int64(maxBucketSpanNanos)))
		}
		capT = int64(capacity) * nanosPerToken
	}

	b := &AtomicTokenBucket{
		nanosPerToken: nanosPerToken,
		capT:          capT,
		capacity:      int64(capacity),
		origin:        clk.Now(),
		clk:           clk,
	}
	if sc, ok := clk.(SinceClock); ok {
		b.sinceClk = sc
	}
	// A full bucket is a virtual zero time one full span in the past.
	b.vt.Store(-capT)
	return b
}

// Allow reports whether one request is permitted now.
func (b *AtomicTokenBucket) Allow() bool { return b.AllowN(1) }

// AllowN attempts to consume n units at once. The batch is all-or-nothing: if
// less than n units of credit are available, nothing is consumed and AllowN
// returns false. For n <= 0 it returns true without touching any state, per the
// Limiter port contract.
//
// # Shape of the loop
//
// Four properties of the code below are deliberate and each was measured to
// matter (RESEARCH §1.3–§1.5); a change that drops any of them gives back a
// large multiple of the cost:
//
//  1. Every field the transition needs is hoisted into a local before the loop.
//     This is the least obvious and the largest effect. LOCK CMPXCHG is a full
//     barrier, so without hoisting the compiler must re-read those fields on
//     every iteration, from the very cache line the contending writers keep
//     invalidating. The loop body below touches no field of b except the CAS
//     target itself.
//  2. The read-only fields sit two cache lines away from vt (see the type
//     comment), so that the hoisted loads before the loop, and everything else
//     in the struct, are not on the contended line either.
//  3. The clock is read once, before the loop, and a retry reuses that reading
//     rather than taking a fresh one. Retries are exactly what contention
//     multiplies, so a clock read inside the loop is paid for again on every
//     lost CAS. Reusing the older reading is also the conservative direction:
//     an older now credits less, never more, and the call linearizes at its own
//     reading, which the Clock contract permits.
//  4. The state is one word, so a transition allocates nothing at all, on any
//     path, however many times the CAS is lost.
//
// There is no backoff and no retry cap: a lost CAS means another goroutine
// committed its transition, so the system made progress, and the retry costs
// one load plus one pure recomputation. That is the Этап 4 decision, unchanged.
//
// # How this behaves against the TokenBucket baseline
//
// The whole port contract matches: all-or-nothing batches, n <= 0 touching
// nothing, a bucket that starts full, credit clamped at capacity, and partial
// accrual carried across calls. The baseline's own scenario table passes
// unchanged for any rate with an integral emission interval.
//
// Three divergences, all intended:
//
//   - A denied call returns false without a CAS, as in LockFreeTokenBucket, so
//     denial writes no state. Here that is not merely equivalent up to a
//     rounding threshold — it is indistinguishable, full stop. Credit is
//     re-derived from (nowNs - vt) in exact integer arithmetic on every call and
//     the capacity clamp is exactly associative over the integers, so the
//     deny-with-CAS and deny-without-CAS orders of operation cannot round to
//     different verdicts. Not persisting accrual on denial is also what
//     x/time/rate does (RESEARCH §3.1).
//   - The enforced rate is quantized to 1e9/round(1e9/rate); see the
//     constructor. Against a float64 baseline the admission counts over a long
//     run differ within that bound.
//   - admitEpsilon is not used and must not be added "for consistency". It
//     exists to absorb float64 drift, and this scheme has none to absorb — every
//     hot-path value is an integer nanosecond count. Adding a slack here would
//     not compensate an error, it would hand out real credit.
//
// # Why ABA cannot bite
//
// LockFreeTokenBucket rules ABA out through the GC (a live snapshot's address
// cannot be recycled). This limiter CASes a plain integer, so that argument is
// unavailable — and unnecessary: on the admitting path next = eff + need with
// eff >= vt and need >= T >= 1, so vt is strictly increasing over the life of
// the bucket and a previously observed value can never reappear.
func (b *AtomicTokenBucket) AllowN(n int) bool {
	if n <= 0 {
		return true // port contract: degenerate batch, state untouched
	}
	if int64(n) > b.capacity {
		// Can never fit, even from full. This also bounds need below by capT,
		// which is what keeps the loop arithmetic inside int64.
		return false
	}

	// Hoist everything the loop reads. See item 1 above; this is not style.
	need := int64(n) * b.nanosPerToken
	capT := b.capT
	nowNs := b.elapsedNanos() // one clock read, before the loop (item 3)

	vt := b.vt.Load()
	for {
		next, ok := nextAtomicState(vt, nowNs, capT, need)
		if !ok {
			return false // no credit for the batch — deny without a CAS
		}
		if b.vt.CompareAndSwap(vt, next) {
			return true
		}
		vt = b.vt.Load() // lost the CAS — retry on fresh state, same nowNs
	}
}

// nextAtomicState is the pure transition function of the CAS loop: given the
// observed virtual zero time, the current instant, the full-bucket span and the
// cost of the batch (all in nanoseconds), it returns the virtual zero time to
// install and whether the batch was admitted. It has no side effects and
// touches no shared state — all the nondeterminism lives in the loop, which is
// what makes the stress test a check of the coordination rather than of the
// arithmetic.
//
// The clamp is the capacity cap in the time domain: a vt further back than one
// full span would credit more than a full bucket, so it is pulled forward to
// the floor first. The admission comparison is inclusive, written as
// `avail < need -> deny`, so advancing time by exactly K emission intervals
// admits exactly K units, matching the baseline.
//
// Every intermediate stays inside int64 given the constructor's bounds:
// nowNs >= 0 and capT <= 1<<62 put the floor no lower than -(1<<62); avail is
// at most capT; and need is at most capT because AllowN rejects n > capacity
// before calling. A concurrent caller whose clock read later may have installed
// a vt beyond our nowNs, which makes avail slightly negative — a correct denial,
// bounded by the spread between two clock readings, and nowhere near an
// underflow.
//
// nowNs >= 0 is a precondition of this function, not something it enforces —
// elapsedNanos is what clamps it, as a defensive measure against a Clock that
// violates its contract (see the comment there).
func nextAtomicState(vt, nowNs, capT, need int64) (next int64, ok bool) {
	eff := vt
	if floor := nowNs - capT; eff < floor {
		eff = floor // an older vt would credit more than a full bucket
	}
	if avail := nowNs - eff; avail < need {
		return 0, false
	}
	return eff + need, true
}

// elapsedNanos returns the nanoseconds elapsed since the bucket's origin,
// preferring the monotonic-only fast path when the injected Clock offers one.
// The fast path skips the wall-clock read that a full Now also does, which is
// a large fraction of the whole operation here — see the Baseline_ClockNow and
// Baseline_ClockSince benchmarks in limiter_bench_test.go for the actual ratio
// on your machine and build; it is not restated as a number here because it
// does not travel between machines or builds. The branch is on a field
// resolved once in the constructor, not a type assertion per call.
//
// The result is clamped at 0 as a defensive measure: nextAtomicState's
// overflow argument assumes nowNs >= 0, which holds for any well-behaved
// Clock (elapsed-since-origin is never negative) but is not otherwise
// enforced. This does not relax the Clock contract — Now (and Since) must
// still be non-decreasing — it only stops a contract violation from
// underflowing the arithmetic into a wrong admission instead of failing safe.
func (b *AtomicTokenBucket) elapsedNanos() int64 {
	var ns int64
	if b.sinceClk != nil {
		ns = int64(b.sinceClk.Since(b.origin))
	} else {
		ns = b.clk.Now().Sub(b.origin).Nanoseconds()
	}
	if ns < 0 {
		return 0
	}
	return ns
}
