package limiter

// TokenBucket is a mutex-guarded token-bucket rate limiter.
//
// Tokens refill continuously at `rate` per second up to `capacity`; each allowed
// request consumes one token. This is the straightforward, obviously-correct
// baseline implementation — the lock-free CAS variant added in Этап 4
// (LockFreeTokenBucket) is benchmarked against it to show what lock-free buys
// and what it costs in complexity.
//
// TODO(Этап 1): implement mutex-based token bucket (refill math + Allow/AllowN),
// wire the injected Clock, add correctness + -race tests.
type TokenBucket struct {
	_ struct{} // placeholder; real fields land in Этап 1
}

// Compile-time assertion that TokenBucket satisfies the Limiter contract.
// Kept even while the body is a stub so the interface can't silently drift.
var _ Limiter = (*TokenBucket)(nil)

// Allow reports whether one request is permitted now.
//
// TODO(Этап 1): implement.
func (b *TokenBucket) Allow() bool { return b.AllowN(1) }

// AllowN attempts to consume n tokens at once.
//
// TODO(Этап 1): implement.
func (b *TokenBucket) AllowN(n int) bool {
	_ = n
	panic("TokenBucket.AllowN: not implemented (Этап 1)")
}
