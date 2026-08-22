package limiter

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The scenarios reuse the step/allow helpers defined in tokenbucket_test.go.

func TestLeakyBucket_Scenarios(t *testing.T) {
	tests := []struct {
		name     string
		rate     float64
		capacity int
		steps    []step
	}{
		{
			name:     "empty bucket fills to capacity then overflows",
			rate:     1,
			capacity: 3,
			steps: []step{
				allow(true), allow(true), allow(true),
				allow(false), allow(false), // full, zero elapsed → no room
			},
		},
		{
			name:     "steady-state at exactly the leak rate is stable",
			rate:     2, // drains one unit every 500ms
			capacity: 1,
			steps: []step{
				allow(true), // level 0 → 1, bucket full
				{advance: 500 * time.Millisecond, n: 1, want: true},
				{advance: 500 * time.Millisecond, n: 1, want: true},
				{advance: 500 * time.Millisecond, n: 1, want: true},
				{advance: 500 * time.Millisecond, n: 1, want: true},
				allow(false), // no elapsed time → still full
			},
		},
		{
			name:     "level drains over time making room again",
			rate:     1,
			capacity: 2,
			steps: []step{
				{n: 2, want: true}, // fill to capacity
				allow(false),
				{advance: time.Second, n: 1, want: true}, // level 2→1, pour back to 2
				allow(false),
				{advance: 2 * time.Second, n: 2, want: true}, // full drain, full pour
				allow(false),
			},
		},
		{
			name:     "burst larger than capacity is denied even on an empty bucket",
			rate:     1,
			capacity: 5,
			steps: []step{
				{n: 6, want: false}, // can never fit
				{n: 5, want: true},  // the denied batch poured nothing
				allow(false),
			},
		},
		{
			name:     "failed AllowN batch pours nothing",
			rate:     1,
			capacity: 5,
			steps: []step{
				{n: 3, want: true},  // level 3
				{n: 4, want: false}, // 3+4 > 5 → all-or-nothing, still 3
				{n: 2, want: true},  // prior level intact: exactly 2 units of room
				allow(false),
			},
		},
		{
			name:     "AllowN zero and negative always allowed without pouring",
			rate:     1,
			capacity: 2,
			steps: []step{
				{n: 0, want: true},
				{n: -1, want: true},
				{n: 2, want: true}, // the full capacity was still available
				{n: 0, want: true}, // true even when the bucket is full
				{n: -5, want: true},
				allow(false), // and the bucket is still full
			},
		},
		{
			// The first advance is denied, yet it must still commit both the
			// leak and the timestamp — otherwise the second advance would
			// recompute from the original mark and stay denied too.
			name:     "denied call still commits the leak and the timestamp",
			rate:     1,
			capacity: 1,
			steps: []step{
				allow(true),
				{advance: 500 * time.Millisecond, n: 1, want: false}, // level 0.5 → no room for 1
				{advance: 500 * time.Millisecond, n: 1, want: true},  // level 0.0 → room again
				allow(false),
			},
		},
		{
			name:     "long idle clamps the level at zero, not below",
			rate:     1,
			capacity: 3,
			steps: []step{
				{n: 3, want: true}, // fill to capacity
				// 5000s idle drains far more than the level; room is still
				// exactly capacity, never "negative water" as extra credit.
				{advance: 5000 * time.Second, n: 3, want: true},
				allow(false),
			},
		},
		{
			name:     "zero rate never drains: fixed total budget of capacity",
			rate:     0,
			capacity: 2,
			steps: []step{
				allow(true), allow(true),
				{advance: 1000 * time.Second, n: 1, want: false}, // no leak, ever
				{n: 0, want: true},
			},
		},
		{
			// A negative rate is clamped to "no leak": the level must not rise
			// spontaneously. Without the clamp, 10s at rate -1 would push the
			// level from 1 to 11 and deny the admissible request below.
			name:     "negative rate behaves like zero rate, level never rises on its own",
			rate:     -1,
			capacity: 2,
			steps: []step{
				allow(true), // level 1
				{advance: 10 * time.Second, n: 1, want: true}, // level stays 1 → room for 1
				allow(false), // full at 2
			},
		},
		{
			name:     "zero capacity denies every positive batch",
			rate:     1,
			capacity: 0,
			steps: []step{
				allow(false),
				{n: 0, want: true}, // degenerate n still short-circuits to true
				{advance: time.Second, n: 1, want: false}, // draining creates no room
			},
		},
		{
			name:     "negative capacity denies every positive batch",
			rate:     1,
			capacity: -3,
			steps: []step{
				allow(false),
				{n: -1, want: true},
				{advance: time.Second, n: 1, want: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			b := NewLeakyBucket(tt.rate, tt.capacity, clk)
			for i, s := range tt.steps {
				if s.advance > 0 {
					clk.Advance(s.advance)
				}
				if got := b.AllowN(s.n); got != s.want {
					t.Fatalf("step %d: AllowN(%d) = %v, want %v", i, s.n, got, s.want)
				}
			}
		})
	}
}

// TestLeakyBucket_AllowNNonPositiveKeepsLeakTimestamp pins the stricter part of
// the contract: AllowN(n <= 0) must not touch state at all — including the
// leak timestamp. If it did update last = now without draining, the elapsed
// time would be lost and the level would stay spuriously high; so we verify
// that no observable state transition happens between an AllowN(0) call and
// the next real call.
func TestLeakyBucket_AllowNNonPositiveKeepsLeakTimestamp(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLeakyBucket(1, 1, clk)

	if !b.Allow() {
		t.Fatal("initial Allow() = false, want true (bucket starts empty)")
	}

	// Advance half a drain, then poke with non-positive n. The 0.5 pending
	// units of leakage must remain creditable to the next real call.
	clk.Advance(500 * time.Millisecond)
	if !b.AllowN(0) {
		t.Fatal("AllowN(0) = false, want true")
	}
	if !b.AllowN(-1) {
		t.Fatal("AllowN(-1) = false, want true")
	}

	clk.Advance(500 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() after a full second of total leakage = false, want true")
	}
	if b.Allow() {
		t.Fatal("extra Allow() = true, want false (only one unit drained)")
	}
}

// TestLeakyBucket_SteadyStateNonBinaryExactStep is the regression test for
// float64 drift in the admission comparison. The table case above checks
// steady state with rate 2 and 500ms steps, where 0.5*2 == 1.0 is binary-exact
// and no residue can accumulate — so it passes with or without admitEpsilon.
//
// Here 0.1 has no exact float64 representation: draining a whole unit as ten
// 100ms leaks leaves ~1.4e-16 behind, and a strict `level+n <= capacity` then
// denies every boundary request. Measured before the fix: 910 admissions out
// of the 1000 that are arithmetically due, i.e. ~9% under the configured rate.
func TestLeakyBucket_SteadyStateNonBinaryExactStep(t *testing.T) {
	const (
		steps = 10000                  // 1000s of simulated time
		step  = 100 * time.Millisecond // rate*step == 0.1 units, not binary-exact
		want  = steps / 10             // one unit drains every ten steps
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLeakyBucket(1, 1, clk)

	allowed := 0
	for i := 0; i < steps; i++ {
		clk.Advance(step)
		if b.Allow() {
			allowed++
		}
	}

	if allowed != want {
		t.Fatalf("admitted %d requests over %v, want exactly %d (client calls at exactly the leak rate); float drift in the admission comparison?",
			allowed, time.Duration(steps)*step, want)
	}
}

// TestLeakyBucket_ConcurrentWithLeak races the leak path itself, which
// TestLeakyBucket_ConcurrentNoOverAdmit below cannot: that test freezes time,
// so `leaked > 0` is never true and the level-draining line never executes
// under concurrency. Here the clock moves while workers hammer Allow.
func TestLeakyBucket_ConcurrentWithLeak(t *testing.T) {
	const (
		capacity   = 50
		rate       = 100.0 // units per second
		goroutines = 16
		callsPer   = 200
		ticks      = 20
		tick       = 10 * time.Millisecond
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLeakyBucket(rate, capacity, clk)

	var admitted atomic.Int64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPer; j++ {
				if b.Allow() {
					admitted.Add(1)
				}
			}
		}()
	}

	// Drive the clock from its own goroutine, interleaved with the workers, so
	// the drain arithmetic actually runs concurrently with admission decisions.
	// Ticking inline here instead would race the workers to finish and usually
	// win, leaving the leak path exercised single-threaded after all.
	done := make(chan struct{})
	var driver sync.WaitGroup
	driver.Add(1)
	go func() {
		defer driver.Done()
		for i := 0; i < ticks; i++ {
			clk.Advance(tick)
			select {
			case <-done:
				// Workers finished early: burn the remaining ticks at once so
				// the elapsed time in the bound below stays exact.
				for j := i + 1; j < ticks; j++ {
					clk.Advance(tick)
				}
				return
			default:
			}
			runtime.Gosched()
		}
	}()

	wg.Wait()
	close(done)
	driver.Wait()

	// Upper bound, not an exact count: the interleaving of Advance and Allow is
	// nondeterministic, but no schedule can admit more than a full bucket plus
	// whatever drained away over the total elapsed time.
	elapsed := time.Duration(ticks) * tick
	bound := int64(capacity + rate*elapsed.Seconds())
	got := admitted.Load()
	if got > bound {
		t.Fatalf("admitted %d requests, want <= %d (capacity %d plus %v of leak at %v/s)", got, bound, capacity, elapsed, rate)
	}
	// Lower bound too, or a limiter that denies everything would pass: the
	// bucket starts empty and there are far more calls than capacity, so at
	// least a full bucket's worth must get through under any schedule.
	if got < capacity {
		t.Fatalf("admitted %d requests, want >= %d (an initially empty bucket must admit at least its capacity)", got, capacity)
	}
}

// TestLeakyBucket_ConcurrentNoOverAdmit hammers one bucket from many goroutines
// with frozen time (leak = 0), so the total number of admitted requests can
// never exceed the capacity. Run with -race this also exercises the mutex path
// for data races.
func TestLeakyBucket_ConcurrentNoOverAdmit(t *testing.T) {
	const (
		capacity   = 100
		goroutines = 16
		callsPer   = 200
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLeakyBucket(1, capacity, clk)

	var admitted atomic.Int64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPer; j++ {
				if b.Allow() {
					admitted.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := admitted.Load(); got != capacity {
		t.Fatalf("admitted %d requests, want exactly %d (time frozen, no leak)", got, capacity)
	}
}
