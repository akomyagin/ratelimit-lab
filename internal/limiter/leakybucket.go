package limiter

// LeakyBucket is a leaky-bucket rate limiter modelled as a queue that drains
// ("leaks") at a fixed rate. A request is admitted only if the bucket has room
// under `capacity`; the steady-state admission rate is capped at the leak rate,
// which smooths bursts into a constant outflow.
//
// TODO(Этап 3): implement leaky bucket (leak math as-a-meter, so no background
// goroutine is required), wire the injected Clock, add correctness + -race tests.
type LeakyBucket struct {
	_ struct{} // placeholder; real fields land in Этап 3
}

// Compile-time assertion that LeakyBucket satisfies the Limiter contract.
var _ Limiter = (*LeakyBucket)(nil)

// Allow reports whether one request is permitted now.
//
// TODO(Этап 3): implement.
func (b *LeakyBucket) Allow() bool { return b.AllowN(1) }

// AllowN attempts to admit n requests at once.
//
// TODO(Этап 3): implement.
func (b *LeakyBucket) AllowN(n int) bool {
	_ = n
	panic("LeakyBucket.AllowN: not implemented (Этап 3)")
}
