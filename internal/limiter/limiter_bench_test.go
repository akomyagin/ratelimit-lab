package limiter

import (
	"testing"
	"time"
)

// Micro-benchmarks for every Limiter implementation (Этап 5).
//
// Every algorithm goes through the same two helpers, benchSerial and
// benchParallel, so the comparison measures the limiters and nothing else: one
// loop body, one load profile, one level of interface indirection. Splitting
// the benchmarks per algorithm would duplicate the loop and let the variants
// drift apart on the first edit. The Baseline_* benchmarks at the bottom give
// the floor those numbers sit on.
//
// Two setup rules make the numbers mean what they claim:
//
//   - SystemClock, never the fakeClock from clock_test.go. The fake takes a
//     mutex on every Now(), which under b.RunParallel serializes every goroutine
//     on the clock itself — the benchmark would compare fakeClock against
//     fakeClock instead of mutex against CAS. Determinism is a correctness-test
//     concern, not a measurement one.
//
//   - The bucket must never run dry. Both lock-free variants return false on a
//     denial without performing a CAS (and without allocating a state
//     snapshot), while the mutex implementations still take the lock on a
//     denial. Benchmarking an empty bucket would therefore time the lock-free
//     fast path against the baseline's full lock acquisition and hand the
//     lock-free variants an unearned win. Rate and capacity are set far above
//     anything b.N can consume so that every call takes the admission path.
//
// ReportAllocs is mandatory here: the Этап 4 state representation
// (atomic.Pointer to an immutable snapshot) costs one allocation per successful
// CAS iteration, and retries under contention multiply it. That price is the
// headline result of this stage, so allocs/op belongs next to ns/op.
//
// Only Allow is benchmarked; AllowN(1) is the same code path by construction.
const (
	// benchRate refills faster than any benchmark loop can consume.
	benchRate = 1e9
	// benchCapacity keeps the bucket effectively bottomless.
	benchCapacity = 1 << 30
	// benchWindowLimit exceeds the largest plausible b.N (~1e9) by three orders
	// of magnitude, so the sliding window never rolls over mid-run and the hot
	// admission path is what gets timed. 1<<40 is exactly representable in
	// float64, so the weighted estimate stays exact.
	benchWindowLimit = 1 << 40
	// benchWindow is long enough that a benchmark run stays inside one window.
	benchWindow = time.Hour
)

// benchSerial times Allow from a single goroutine: the uncontended cost of one
// admission decision.
func benchSerial(b *testing.B, lim Limiter) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lim.Allow()
	}
}

// benchParallel times Allow from GOMAXPROCS goroutines hammering one limiter:
// the contended cost, which is where mutex and CAS diverge.
func benchParallel(b *testing.B, lim Limiter) {
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lim.Allow()
		}
	})
}

func BenchmarkTokenBucket_Serial(b *testing.B) {
	benchSerial(b, NewTokenBucket(benchRate, benchCapacity, SystemClock))
}

func BenchmarkTokenBucket_Parallel(b *testing.B) {
	benchParallel(b, NewTokenBucket(benchRate, benchCapacity, SystemClock))
}

func BenchmarkSlidingWindow_Serial(b *testing.B) {
	benchSerial(b, NewSlidingWindow(benchWindowLimit, benchWindow, SystemClock))
}

func BenchmarkSlidingWindow_Parallel(b *testing.B) {
	benchParallel(b, NewSlidingWindow(benchWindowLimit, benchWindow, SystemClock))
}

func BenchmarkLeakyBucket_Serial(b *testing.B) {
	benchSerial(b, NewLeakyBucket(benchRate, benchCapacity, SystemClock))
}

func BenchmarkLeakyBucket_Parallel(b *testing.B) {
	benchParallel(b, NewLeakyBucket(benchRate, benchCapacity, SystemClock))
}

func BenchmarkLockFreeTokenBucket_Serial(b *testing.B) {
	benchSerial(b, NewLockFreeTokenBucket(benchRate, benchCapacity, SystemClock))
}

func BenchmarkLockFreeTokenBucket_Parallel(b *testing.B) {
	benchParallel(b, NewLockFreeTokenBucket(benchRate, benchCapacity, SystemClock))
}

// AtomicTokenBucket runs on the same constants as the others, and they all fall
// well inside its integer bounds: benchRate 1e9 gives an emission interval of
// exactly 1 ns (no quantization error to explain away), and benchCapacity 1<<30
// makes the bucket span ~1.07e9 ns, nine orders of magnitude below the limit.
// It starts full with 2^30 units and accrues 1e9/s against a consumption of
// roughly 1e7/s, so it never runs dry and every call takes the admission path,
// as required above. SystemClock implements SinceClock, so this measures the
// production configuration including the monotonic-only clock read.
func BenchmarkAtomicTokenBucket_Serial(b *testing.B) {
	benchSerial(b, NewAtomicTokenBucket(benchRate, benchCapacity, SystemClock))
}

func BenchmarkAtomicTokenBucket_Parallel(b *testing.B) {
	benchParallel(b, NewAtomicTokenBucket(benchRate, benchCapacity, SystemClock))
}

// Baselines: the floor under every number above, and the way to read them.
//
// A serial admission decision is dominated by things that are not the limiter.
// Baseline_NoopLimiter is the loop plus one interface dispatch and nothing else;
// Baseline_ClockNow and Baseline_ClockSince are the two ways a limiter can ask
// what time it is. The measured clock cost is a large fraction of the fastest
// limiter's serial time — around 73 ns for Since against 113 for Now, versus a
// whole admission in under 100 (RESEARCH §1.5) — so a serial figure read
// without subtracting this floor mostly describes the clock.
//
// Subtract the floor from the serial figures only. Under b.RunParallel the
// picture inverts: contention dominates, the clock falls to a few percent of
// the profile, and subtracting a single-threaded floor from a contended number
// means nothing.
//
// Baseline_ClockNow minus Baseline_ClockSince is the price the Clock port used
// to charge every limiter unconditionally, and what the optional SinceClock
// extension removes for those that ask for it.
type noopLimiter struct{}

func (noopLimiter) Allow() bool     { return true }
func (noopLimiter) AllowN(int) bool { return true }

func BenchmarkBaseline_NoopLimiter_Serial(b *testing.B) { benchSerial(b, noopLimiter{}) }

func BenchmarkBaseline_NoopLimiter_Parallel(b *testing.B) { benchParallel(b, noopLimiter{}) }

// Package-level sinks so the compiler cannot discard the clock reads being
// timed. They are never read; that is the point.
var (
	sinkTime    time.Time
	sinkElapsed time.Duration
)

func BenchmarkBaseline_ClockNow(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkTime = SystemClock.Now()
	}
}

func BenchmarkBaseline_ClockSince(b *testing.B) {
	sc, ok := SystemClock.(SinceClock)
	if !ok {
		b.Skip("SystemClock does not implement SinceClock")
	}
	base := sc.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkElapsed = sc.Since(base)
	}
}
