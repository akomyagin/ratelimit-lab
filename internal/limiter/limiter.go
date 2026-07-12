// Package limiter defines the common rate-limiter contract and its
// single-process algorithm implementations (token bucket, sliding window,
// leaky bucket). All implementations must be safe for concurrent use by
// multiple goroutines.
package limiter

import "time"

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
type Clock interface {
	Now() time.Time
}

// realClock is the default wall-clock implementation backed by time.Now.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// SystemClock is the process-wide real clock used by default constructors.
var SystemClock Clock = realClock{}
