// Package limiter defines the common rate-limiter contract and its
// single-process algorithm implementations (token bucket, sliding window,
// leaky bucket). All implementations must be safe for concurrent use by
// multiple goroutines.
package limiter

import "time"

// admitEpsilon absorbs float64 rounding error in the admission comparison so
// that exact boundary cases (e.g. a level arithmetically equal to the limit)
// are not spuriously denied by a few ULPs of drift.
//
// It is shared by every implementation whose state accrues over time as a
// float64, because the drift is measurable rather than theoretical: a client
// calling at exactly the configured rate with a step that is not binary-exact
// (rate 1/s polled every 100ms, since 0.1 has no exact float64 representation)
// leaves a residue of ~1e-16 after draining what should be a whole unit. Every
// boundary request is then denied and throughput lands ~9% under the configured
// rate. Measured on both TokenBucket and LeakyBucket: 910 admissions where 1000
// were due.
//
// 1e-9 is far above the accumulated drift (~1e-16 per unit) and far below any
// meaningful fraction of a request, so it fixes the boundary without loosening
// the limit in any way an observer could notice: the debt an admission can run
// up is bounded by one epsilon for the lifetime of the limiter, not per call.
//
// The absolute constant stops working once the limit reaches 2^24 (16777216):
// float64 spacing there is 3.7e-9, so `capacity+admitEpsilon == capacity` and
// the epsilon silently becomes a no-op, restoring the boundary denials it was
// added to prevent. That is accepted rather than fixed with a relative epsilon —
// the limits in this project count requests, and 16M concurrent requests is not
// a configuration worth complicating the arithmetic for. Revisit if a limiter is
// ever used to meter bytes.
const admitEpsilon = 1e-9

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

// realClock is the default wall-clock implementation backed by time.Now.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// SystemClock is the process-wide real clock used by default constructors.
var SystemClock Clock = realClock{}
