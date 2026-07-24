package limiter

import (
	"sync"
	"time"
)

// admitEpsilon absorbs float64 rounding error in the admission comparison so
// that exact boundary cases (e.g. estimate arithmetically equal to the limit)
// are not spuriously denied by a few ULPs of drift.
const admitEpsilon = 1e-9

// SlidingWindow is a mutex-guarded weighted sliding-window-counter rate
// limiter: it admits at most `limit` requests within any trailing `window`
// duration, avoiding the burst-at-boundary flaw of a naive fixed-window
// counter.
//
// Instead of logging every request timestamp (O(requests) memory), it keeps
// just two counters — the previous and the current fixed window — and
// estimates the trailing-window usage as
//
//	estimate = prevCount*overlap + currCount
//
// where overlap is the fraction of the previous fixed window still covered by
// the sliding interval ending now. This assumes requests in the previous
// window were uniformly distributed, so it is an O(1)-memory approximation
// rather than an exact count — the accepted trade-off documented in
// TECHNICAL_PLAN §5/§6 (Этап 2).
//
// SlidingWindow is safe for concurrent use by multiple goroutines.
type SlidingWindow struct {
	mu sync.Mutex

	windowStart time.Time // start of the current fixed window
	prevCount   float64   // admitted units in the previous fixed window
	currCount   float64   // admitted units in the current fixed window

	limit  int           // max admitted units per trailing window
	window time.Duration // sliding window length

	clk Clock
}

// Compile-time assertion that SlidingWindow satisfies the Limiter contract.
var _ Limiter = (*SlidingWindow)(nil)

// NewSlidingWindow returns a SlidingWindow admitting at most limit requests
// per trailing window. The first fixed window starts at clk.Now(). Pass
// SystemClock in production code; tests inject a fake Clock.
//
// NewSlidingWindow panics if window is not positive: the window-rollover math
// divides elapsed time by window, so a non-positive window is a
// misconfiguration, not a valid degenerate case (mirrors time.NewTicker).
func NewSlidingWindow(limit int, window time.Duration, clk Clock) *SlidingWindow {
	if window <= 0 {
		panic("limiter: NewSlidingWindow: non-positive window")
	}
	return &SlidingWindow{
		windowStart: clk.Now(),
		limit:       limit,
		window:      window,
		clk:         clk,
	}
}

// Allow reports whether one request is permitted now.
func (w *SlidingWindow) Allow() bool { return w.AllowN(1) }

// AllowN attempts to admit n requests at once. The batch is all-or-nothing: if
// the weighted estimate plus n would exceed the limit, nothing is counted and
// AllowN returns false. For n <= 0 it returns true without touching any state,
// per the Limiter port contract.
func (w *SlidingWindow) AllowN(n int) bool {
	if n <= 0 {
		return true
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.clk.Now()
	elapsed := now.Sub(w.windowStart)

	// Roll fixed windows forward if the current one has ended. After one full
	// window the current counter becomes the previous one; after two or more
	// the previous window carries no weight at all.
	if elapsed >= w.window {
		windowsPassed := int(elapsed / w.window)
		if windowsPassed == 1 {
			w.prevCount = w.currCount
		} else {
			w.prevCount = 0
		}
		w.currCount = 0
		w.windowStart = w.windowStart.Add(time.Duration(windowsPassed) * w.window)
		elapsed = now.Sub(w.windowStart) // now within [0, window)
	}

	// overlap is the fraction of the previous fixed window still inside the
	// sliding interval (now-window, now].
	overlap := float64(w.window-elapsed) / float64(w.window)
	estimate := w.prevCount*overlap + w.currCount

	if estimate+float64(n) <= float64(w.limit)+admitEpsilon {
		w.currCount += float64(n)
		return true
	}
	return false
}
