package limiter

import (
	"testing"
	"time"
)

// Micro-benchmarks for every Limiter implementation (Этап 5).
//
// All four algorithms go through the same two helpers, benchSerial and
// benchParallel, so the comparison measures the limiters and nothing else: one
// loop body, one load profile, one level of interface indirection. Splitting
// the benchmarks per algorithm would duplicate the loop and let the variants
// drift apart on the first edit.
//
// Two setup rules make the numbers mean what they claim:
//
//   - SystemClock, never the fakeClock from clock_test.go. The fake takes a
//     mutex on every Now(), which under b.RunParallel serializes every goroutine
//     on the clock itself — the benchmark would compare fakeClock against
//     fakeClock instead of mutex against CAS. Determinism is a correctness-test
//     concern, not a measurement one.
//
//   - The bucket must never run dry. LockFreeTokenBucket returns false on a
//     denial without performing a CAS (and without allocating a state
//     snapshot), while the mutex implementations still take the lock on a
//     denial. Benchmarking an empty bucket would therefore time the lock-free
//     fast path against the baseline's full lock acquisition and hand the
//     lock-free variant an unearned win. Rate and capacity are set far above
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
