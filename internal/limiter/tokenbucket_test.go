package limiter

import (
	"sync"
	"testing"
	"time"
)

// step is one scripted action against the bucket: optionally advance the fake
// clock, then call AllowN(n) and check the verdict.
type step struct {
	advance time.Duration
	n       int
	want    bool
}

// allow is shorthand for a step that calls AllowN(1) with no time advance.
func allow(want bool) step { return step{n: 1, want: want} }

func TestTokenBucket_Scenarios(t *testing.T) {
	tests := []struct {
		name     string
		rate     float64
		capacity int
		steps    []step
	}{
		{
			name:     "full bucket allows capacity requests then denies",
			rate:     1,
			capacity: 3,
			steps: []step{
				allow(true), allow(true), allow(true),
				allow(false),
			},
		},
		{
			name:     "empty bucket with zero elapsed denies",
			rate:     10,
			capacity: 2,
			steps: []step{
				allow(true), allow(true), // drain the initially full bucket
				allow(false), allow(false), // time never advanced → still empty
			},
		},
		{
			name:     "advance worth exactly K tokens allows exactly K requests",
			rate:     2, // 2 tokens/sec
			capacity: 10,
			steps: []step{
				{n: 10, want: true}, // drain to zero
				allow(false),
				{advance: 3 * time.Second, n: 1, want: true}, // K = 2*3 = 6 tokens
				allow(true), allow(true), allow(true), allow(true), allow(true),
				allow(false), // 7th request after refill of 6 must be denied
			},
		},
		{
			name:     "refill clamps at capacity after long idle",
			rate:     1,
			capacity: 5,
			steps: []step{
				{n: 5, want: true}, // drain to zero
				// 5000s idle = 1000x the full-refill time; still only 5 tokens.
				{advance: 5000 * time.Second, n: 5, want: true},
				allow(false),
			},
		},
		{
			name:     "failed AllowN batch consumes nothing",
			rate:     1,
			capacity: 5,
			steps: []step{
				{n: 2, want: true},  // 3 tokens left
				{n: 4, want: false}, // over budget → all-or-nothing, still 3 left
				{n: 3, want: true},  // prior level intact: exactly 3 available
				allow(false),
			},
		},
		{
			name:     "AllowN zero and negative always allowed without spending",
			rate:     1,
			capacity: 2,
			steps: []step{
				{n: 0, want: true},
				{n: -1, want: true},
				{n: 2, want: true}, // still the full 2 tokens
				{n: 0, want: true}, // true even on an empty bucket
				{n: -5, want: true},
				allow(false), // and the bucket is still empty
			},
		},
		{
			name:     "fractional refill accumulates across advances",
			rate:     1,
			capacity: 1,
			steps: []step{
				allow(true),
				{advance: 500 * time.Millisecond, n: 1, want: false}, // 0.5 tokens
				{advance: 500 * time.Millisecond, n: 1, want: true},  // 1.0 token
				allow(false),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			b := NewTokenBucket(tt.rate, tt.capacity, clk)
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

// TestTokenBucket_AllowNNonPositiveKeepsRefillTimestamp pins the stricter part
// of the contract: AllowN(n <= 0) must not touch state at all — including the
// refill timestamp. If it did recompute refill (last = now), the elapsed time
// would still be credited as tokens, which the scenario below would not catch;
// so instead we verify that no observable state transition happens between an
// AllowN(0) call and the next real call.
func TestTokenBucket_AllowNNonPositiveKeepsRefillTimestamp(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewTokenBucket(1, 1, clk)

	if !b.Allow() {
		t.Fatal("initial Allow() = false, want true (bucket starts full)")
	}

	// Advance half a refill, then poke with non-positive n. The 0.5 pending
	// tokens must remain creditable to the next real call.
	clk.Advance(500 * time.Millisecond)
	if !b.AllowN(0) {
		t.Fatal("AllowN(0) = false, want true")
	}
	if !b.AllowN(-1) {
		t.Fatal("AllowN(-1) = false, want true")
	}

	clk.Advance(500 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() after a full second of total refill = false, want true")
	}
	if b.Allow() {
		t.Fatal("extra Allow() = true, want false (only one token refilled)")
	}
}

// TestTokenBucket_ConcurrentNoOverAdmit hammers one bucket from many goroutines
// with frozen time (refill = 0), so the total number of admitted requests can
// never exceed the initial capacity. Run with -race this also exercises the
// mutex path for data races.
func TestTokenBucket_ConcurrentNoOverAdmit(t *testing.T) {
	const (
		capacity   = 100
		goroutines = 16
		callsPer   = 200
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewTokenBucket(1, capacity, clk)

	var mu sync.Mutex
	allowed := 0

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for j := 0; j < callsPer; j++ {
				if b.Allow() {
					local++
				}
			}
			mu.Lock()
			allowed += local
			mu.Unlock()
		}()
	}
	wg.Wait()

	if allowed != capacity {
		t.Fatalf("admitted %d requests, want exactly %d (time frozen, no refill)", allowed, capacity)
	}
}
