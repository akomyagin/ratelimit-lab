// Package limiter defines the common rate-limiter contract and its
// single-process algorithm implementations: a mutex-guarded token bucket,
// weighted sliding window and leaky bucket, plus two lock-free CAS variants of
// the token bucket that the others serve as a baseline for —
// LockFreeTokenBucket, which CASes a pointer to an immutable snapshot, and
// AtomicTokenBucket, which CASes a single int64 word laid out for the cache.
// All implementations must be safe for concurrent use by multiple goroutines.
package limiter

import "time"

// admitEpsilonRel is the relative slack of the admission comparison: the
// tolerance granted to a request is admitEpsilonRel of the limit it is being
// compared against, not a fixed absolute amount.
//
// The slack exists because state that accrues over time as a float64 drifts,
// measurably rather than theoretically: a client calling at exactly the
// configured rate with a step that is not binary-exact (rate 1/s polled every
// 100ms, since 0.1 has no exact float64 representation) leaves a residue of
// ~1e-16 after draining what should be a whole unit. Every boundary request is
// then denied and throughput lands ~9% under the configured rate. Measured on
// both TokenBucket and LeakyBucket: 910 admissions where 1000 were due.
//
// 1e-9 relative is far above the accumulated drift (~1e-16 per unit of the
// quantity involved) and far below any fraction of a request an observer could
// notice: the debt an admission can run up is bounded by one epsilon of the
// limit for the lifetime of the limiter, not per call. A limit configured to a
// relative precision of 1e-9 is indistinguishable from this one.
//
// Why relative rather than the absolute 1e-9 this started as: an absolute
// constant silently degenerates into a no-op once the compared-against limit
// reaches 2^24 (16777216), where float64 spacing is 3.7e-9 and
// `limit+1e-9 == limit`, which restores exactly the boundary denials the
// epsilon was added to prevent. That ceiling was originally accepted on the
// grounds that the limits here count requests and 16M of them is not a
// realistic configuration. The trigger for revisiting it was stated as "if a
// limiter is ever used to meter volume", and that turned out to be the
// ordinary case rather than a hypothetical: RFC 9110 rate-limit headers meter
// content bytes, Azure API Management meters kilobytes, and Cloudflare admits
// scores up to a million (RESEARCH §2.2). Scaling with the limit removes the
// ceiling without changing any verdict at today's limits — see admitEpsilon.
//
// That fix is complete only on the fill side (SlidingWindow, LeakyBucket),
// where the epsilon is added to the same operand — limit/capacity — that it
// is scaled by. On the spend side (TokenBucket, LockFreeTokenBucket) the
// epsilon is added to the accrued counter (tokens) but scaled by the batch
// size n, because n is the only quantity admitEpsilon's caller has at the
// comparison site; the drift being absorbed lives in tokens, whose magnitude
// is bounded by capacity, not by n. For a large capacity and a small batch —
// e.g. NewTokenBucket(1, 1<<25, clk) admitting with n=1 — the 2^24 ceiling
// this section describes still applies: ULP(2^25) = 4e-9 exceeds the
// n=1 slack of 1e-9, and the drift can again exceed it. This is a known
// limitation of scaling by n on the spend side, not a regression: behavior
// there is unchanged from before this comment was narrowed. Scaling by
// max(n, capacity) instead was considered and rejected — at capacity = 1<<30
// the slack would grow to a full 1.07 tokens, an observable giveaway, which
// is worse than the ceiling it would remove. The complete fix is to store
// time instead of tokens, as AtomicTokenBucket does, where the hot path is
// exact integer arithmetic and admitEpsilon plays no part at all.
const admitEpsilonRel = 1e-9

// admitEpsilon returns the admission slack to grant against limit — the operand
// the request is compared with (the batch size for implementations that spend,
// the capacity or window limit for implementations that fill). See the
// admitEpsilonRel doc comment for the limits of this scheme on the spend side.
//
// For limit == 1, the multiplication is exact and yields the historical 1e-9
// bit for bit, so every Allow() path keeps its previous verdict exactly.
//
// limit is expected to be positive, matching the quantities it is compared
// against in practice. A negative limit (accepted by NewLeakyBucket and
// NewSlidingWindow, which do not validate their limit/capacity argument)
// yields a negative slack, making the comparison marginally stricter than an
// exact one; it does not change any verdict this package's tests exercise.
func admitEpsilon(limit float64) float64 { return limit * admitEpsilonRel }

// Limiter is the common contract shared by every rate-limiting algorithm in
// this package. Implementations decide, on each call, whether one unit of work
// ("one request") is permitted right now under the configured limit.
//
// Every implementation MUST be safe for concurrent use by multiple goroutines.
// The whole point of the project is to compare how different concurrency
// primitives (mutex vs atomic vs lock-free CAS loops) uphold that guarantee
// under load, so a single-threaded-only implementation is a bug, not a variant.
type Limiter interface {
	// Allow reports whether a single request is permitted at the moment of the
	// call, consuming capacity when it returns true. It never blocks: a denied
	// request returns false immediately rather than waiting for capacity.
	Allow() bool

	// AllowN is the batch form of Allow: it attempts to consume n units at once
	// and reports whether the whole batch fit under the limit. AllowN(1) is
	// equivalent to Allow().
	AllowN(n int) bool
}

// Clock abstracts time so tests can drive limiters deterministically instead of
// sleeping. Production code uses realClock; tests inject a controllable fake.
//
// It is introduced in the port here (Этап 0) so that every algorithm added
// later shares one time source and the race/stress tests stay deterministic.
//
// Now must be non-decreasing across successive calls: every implementation's
// refill/leak/window math computes elapsed := now.Sub(last) and assumes
// elapsed >= 0. realClock upholds this via Go's monotonic clock reading inside
// time.Time; a fake Clock used in tests must likewise never be made to go
// backward.
type Clock interface {
	Now() time.Time
}

// SinceClock is an optional extension of Clock: a time source that can measure
// the duration elapsed since an earlier Now() reading without constructing a
// full time.Time. For the system clock this reads only the monotonic clock,
// which is a fixed fraction cheaper than a full Now (which also reads the
// wall clock — RESEARCH §1.5); see the Baseline_ClockNow and
// Baseline_ClockSince benchmarks in limiter_bench_test.go for the actual ratio
// on your machine and build, rather than a number that would not travel to
// either. That fraction matters in a hot path that does nothing else of
// comparable cost.
//
// Contract: Since(t) must agree with Now().Sub(t), so for a t obtained from an
// earlier Now() it is non-negative and non-decreasing, exactly as the Clock
// contract already requires of Now itself.
//
// Clock is deliberately left alone: the interface every limiter is written
// against does not change. Implementations that can exploit the fast path
// detect it once, with a type assertion at construction time, and fall back to
// Now() when the injected Clock does not provide it (as the test fake does, on
// purpose, so both paths stay covered).
type SinceClock interface {
	Clock
	Since(t time.Time) time.Duration
}

// realClock is the default wall-clock implementation backed by time.Now.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// Since returns the time elapsed since t, reading the monotonic clock only.
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

// Compile-time assertion that the production clock offers the fast path, so
// limiters that look for it actually find it in production.
var _ SinceClock = realClock{}

// SystemClock is the process-wide real clock used by default constructors.
var SystemClock Clock = realClock{}
