package limiter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The scenarios reuse the step/allow helpers defined in tokenbucket_test.go.

func TestSlidingWindow_Scenarios(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		window time.Duration
		steps  []step
	}{
		{
			name:   "window filled to limit then denies",
			limit:  3,
			window: time.Second,
			steps: []step{
				allow(true), allow(true), allow(true),
				allow(false), allow(false),
			},
		},
		{
			// Full previous window keeps weighing on the new one: right at the
			// boundary (t=1s) overlap=1.0, estimate = 4*1.0 = 4, so 4+1 > 4 →
			// denied. Capacity frees only as the old window slides out: at
			// t=1.25s overlap=0.75, estimate = 4*0.75 = 3.0, 3+1 <= 4 → one
			// admitted; then estimate = 3+1 = 4 → next denied.
			name:   "capacity frees gradually as previous window slides out",
			limit:  4,
			window: time.Second,
			steps: []step{
				{n: 4, want: true}, // fill window 0 at t=0
				allow(false),
				{advance: time.Second, n: 1, want: false},            // t=1.00s: estimate 4.0
				{advance: 100 * time.Millisecond, n: 1, want: false}, // t=1.10s: estimate 4*0.9  = 3.6
				{advance: 150 * time.Millisecond, n: 1, want: true},  // t=1.25s: estimate 4*0.75 = 3.0
				allow(false), // estimate 3.0+1 = 4.0 → 4+1 > 4
			},
		},
		{
			// Exactly-equal estimate must be admitted (epsilon guard): at
			// t=1.5s overlap=0.5, estimate = 4*0.5 = 2.0, and AllowN(2) gives
			// 2.0+2 = 4.0 = limit → true.
			name:   "batch admitted when estimate lands exactly on the limit",
			limit:  4,
			window: time.Second,
			steps: []step{
				{n: 4, want: true},
				{advance: 1500 * time.Millisecond, n: 2, want: true},
				allow(false), // estimate 2.0+2+1 = 5 > 4
			},
		},
		{
			// After 2+ idle windows prevCount is dropped entirely: the full
			// limit is available again in one burst.
			name:   "two idle windows reset both counters",
			limit:  3,
			window: time.Second,
			steps: []step{
				{n: 3, want: true}, // fill window 0
				allow(false),
				{advance: 2 * time.Second, n: 3, want: true}, // windowsPassed=2 → prev=0
				allow(false),
			},
		},
		{
			name:   "AllowN zero and negative always allowed without counting",
			limit:  2,
			window: time.Second,
			steps: []step{
				{n: 0, want: true},
				{n: -1, want: true},
				{n: 2, want: true}, // full limit still available
				{n: 0, want: true}, // true even when the window is full
				{n: -5, want: true},
				allow(false), // and the window is still full
			},
		},
		{
			name:   "failed AllowN batch counts nothing",
			limit:  5,
			window: time.Second,
			steps: []step{
				{n: 3, want: true},  // currCount = 3
				{n: 4, want: false}, // 3+4 > 5 → all-or-nothing, still 3
				{n: 2, want: true},  // prior level intact: exactly 2 available
				allow(false),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			w := NewSlidingWindow(tt.limit, tt.window, clk)
			for i, s := range tt.steps {
				if s.advance > 0 {
					clk.Advance(s.advance)
				}
				if got := w.AllowN(s.n); got != s.want {
					t.Fatalf("step %d: AllowN(%d) = %v, want %v", i, s.n, got, s.want)
				}
			}
		})
	}
}

// TestSlidingWindow_WindowBoundary pins the key property from TECHNICAL_PLAN
// §6 (Этап 2): a burst at the end of window N must keep weighing on the start
// of window N+1, so the boundary cannot be used to double the budget the way a
// naive fixed-window counter allows.
//
// Numbers, computed against the weighted-counter formula (limit=5, window=1s):
//
//   - t=0.9s: 5 requests admitted (prev=0, curr grows 0→5); a 6th is denied.
//   - t=1.1s: one window rolled (prev=5, curr=0), elapsed=0.1s →
//     overlap = 0.9, estimate = 5*0.9 = 4.5; 4.5+1 = 5.5 > 5 → denied.
//     A fixed-window counter would have admitted 5 fresh requests here,
//     yielding a 10-request burst within the 0.2s straddling the boundary.
//   - t=1.2s: overlap = 0.8, estimate = 5*0.8 = 4.0; 4+1 = 5 <= 5 → exactly
//     one admitted (the epsilon guard covers the exact-equality boundary),
//     then estimate = 4+1 = 5 → the next is denied.
//
// Note this is the approximation at work, not an exact trailing count: the
// formula treats the previous window's 5 requests as uniformly spread over
// [0s,1s), while they actually landed at t=0.9s. The test asserts the real
// behavior of the formula — deny near the boundary, admit gradually as the old
// window's weight decays.
func TestSlidingWindow_WindowBoundary(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	w := NewSlidingWindow(5, time.Second, clk)

	// Burst at the end of window N (t=0.9s).
	clk.Advance(900 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if !w.Allow() {
			t.Fatalf("request %d at t=0.9s = false, want true (window empty)", i)
		}
	}
	if w.Allow() {
		t.Fatal("6th request at t=0.9s = true, want false (limit reached)")
	}

	// Just past the boundary (t=1.1s): estimate = 5*0.9 = 4.5 → still denied.
	clk.Advance(200 * time.Millisecond)
	if w.Allow() {
		t.Fatal("request at t=1.1s = true, want false (estimate 4.5 + 1 > 5)")
	}

	// t=1.2s: estimate = 5*0.8 = 4.0 → exactly one more fits.
	clk.Advance(100 * time.Millisecond)
	if !w.Allow() {
		t.Fatal("request at t=1.2s = false, want true (estimate 4.0 + 1 == 5)")
	}
	if w.Allow() {
		t.Fatal("2nd request at t=1.2s = true, want false (estimate 5.0 + 1 > 5)")
	}
}

// TestSlidingWindow_ConcurrentNoOverAdmit hammers one limiter from many
// goroutines with frozen time inside a single window (prev=0, overlap fixed),
// so the total number of admitted requests must be exactly the limit. Run with
// -race this also exercises the mutex path for data races.
func TestSlidingWindow_ConcurrentNoOverAdmit(t *testing.T) {
	const (
		limit      = 100
		goroutines = 16
		callsPer   = 200
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	w := NewSlidingWindow(limit, time.Second, clk)

	var mu sync.Mutex
	allowed := 0

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for j := 0; j < callsPer; j++ {
				if w.Allow() {
					local++
				}
			}
			mu.Lock()
			allowed += local
			mu.Unlock()
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("admitted %d requests, want exactly %d (time frozen inside one window)", allowed, limit)
	}
}

// TestSlidingWindow_ConcurrentAcrossRollover races the window-rollover path
// itself (windowStart mutation, prev=curr handoff) instead of only the
// steady-state currCount increments: TestSlidingWindow_ConcurrentNoOverAdmit
// above never advances the clock, so it never exercises windowsPassed>=1
// under concurrent access.
func TestSlidingWindow_ConcurrentAcrossRollover(t *testing.T) {
	const (
		limit      = 50
		goroutines = 16
		callsPer   = 200
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	w := NewSlidingWindow(limit, time.Second, clk)

	var admitted atomic.Int64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPer; j++ {
				if w.Allow() {
					admitted.Add(1)
				}
			}
		}()
	}

	// Roll the fixed window forward once while workers are still hammering
	// AllowN, so the prev=curr handoff runs concurrently with admission
	// decisions rather than only ever single-threaded.
	clk.Advance(time.Second)

	wg.Wait()

	// Loose bound rather than an exact count: at most `limit` can be admitted
	// in window 0 before the rollover, and at most `limit` more in window 1
	// afterward (the weighted estimate only ever tightens admission, never
	// relaxes it beyond one window's worth per fixed window touched).
	if got := admitted.Load(); got > 2*limit {
		t.Fatalf("admitted %d requests, want <= %d (at most one limit's worth per window touched across a concurrent rollover)", got, 2*limit)
	}
}

// TestNewSlidingWindow_NonPositiveWindowPanics pins the misconfiguration guard:
// a non-positive window would otherwise divide by zero (or misbehave) inside
// the rollover arithmetic in AllowN, so the constructor panics eagerly with a
// clear message instead (mirrors time.NewTicker).
func TestNewSlidingWindow_NonPositiveWindowPanics(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	for _, window := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewSlidingWindow(_, %v, _) did not panic, want panic on non-positive window", window)
				}
			}()
			NewSlidingWindow(10, window, clk)
		}()
	}
}
