package limiter

// SlidingWindow is a sliding-window-log / sliding-window-counter rate limiter:
// it allows at most `limit` requests within any trailing `window` duration,
// avoiding the burst-at-boundary flaw of a naive fixed-window counter.
//
// TODO(Этап 2): implement sliding window (log or weighted counter — decide and
// document the trade-off in TECHNICAL_PLAN §Этап 2), wire the injected Clock,
// add correctness + -race tests.
type SlidingWindow struct {
	_ struct{} // placeholder; real fields land in Этап 2
}

// Compile-time assertion that SlidingWindow satisfies the Limiter contract.
var _ Limiter = (*SlidingWindow)(nil)

// Allow reports whether one request is permitted now.
//
// TODO(Этап 2): implement.
func (w *SlidingWindow) Allow() bool { return w.AllowN(1) }

// AllowN attempts to admit n requests at once.
//
// TODO(Этап 2): implement.
func (w *SlidingWindow) AllowN(n int) bool {
	_ = n
	panic("SlidingWindow.AllowN: not implemented (Этап 2)")
}
