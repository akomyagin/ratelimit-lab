package limiter

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The scenarios reuse the step/allow helpers defined in tokenbucket_test.go.
// The table mirrors TestTokenBucket_Scenarios case for case: the lock-free
// variant must be observably identical to the mutex baseline, so it must pass
// the baseline's own correctness suite unchanged.

func TestLockFreeTokenBucket_Scenarios(t *testing.T) {
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
		{
			// Same shape as the case above at capacity 2, so it pins that the
			// half refill pending across a denied call is neither lost nor
			// doubled once the bucket can hold more than one token. It is not a
			// lock-free-specific pin and cannot be one: deny-with-CAS and
			// deny-without-CAS are indistinguishable through the Limiter port
			// by construction — that is the very claim the divergence rests on.
			name:     "refill pending across denied calls is credited once, in full",
			rate:     1,
			capacity: 2,
			steps: []step{
				{n: 2, want: true}, // drain to zero
				{advance: 500 * time.Millisecond, n: 1, want: false}, // 0.5 tokens pending, denied without CAS
				{advance: 500 * time.Millisecond, n: 1, want: true},  // full second credited in one shot
				allow(false), // and spent exactly once
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			b := NewLockFreeTokenBucket(tt.rate, tt.capacity, clk)
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

// TestLockFreeTokenBucket_AllowNNonPositiveKeepsRefillTimestamp pins the
// stricter part of the contract, mirroring the mutex baseline's test:
// AllowN(n <= 0) must not touch state at all — including the refill
// timestamp. The 0.5 pending tokens accrued before the non-positive calls
// must remain creditable, exactly once, to the next real call.
func TestLockFreeTokenBucket_AllowNNonPositiveKeepsRefillTimestamp(t *testing.T) {
	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLockFreeTokenBucket(1, 1, clk)

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

// TestNextLockFreeState_AdmitEpsilonCoversFloatResidue is the regression test
// for admitEpsilon in this limiter's spend comparison. It goes straight at the
// pure transition function because no black-box call sequence gets there: the
// deny path does not CAS, so the timestamp only moves on admissions, elapsed
// is always a whole step, and float residue never accrues (see the scope note
// on TestLockFreeTokenBucket_SteadyStateNonBinaryExactStep). Delete
// admitEpsilon from nextLockFreeState and this test is the one that fails.
//
// The two cases bracket the epsilon from both sides: a residue of one ULP must
// be absorbed, and a shortfall three orders of magnitude larger than the
// epsilon must still be denied — so the test also fails if someone "fixes" a
// drift problem by widening admitEpsilon into a free token.
func TestNextLockFreeState_AdmitEpsilonCoversFloatResidue(t *testing.T) {
	now := time.Unix(0, 0)

	// One ULP below 1.0: `tokens >= 1` is false, `tokens+admitEpsilon >= 1` is
	// true. A var, not a const, so the comparison below is a float64 runtime
	// check rather than exact constant arithmetic.
	justUnderOne := math.Nextafter(1, 0)
	if justUnderOne >= 1 {
		t.Fatalf("test setup: tokens = %v is not below 1.0, the case no longer discriminates", justUnderOne)
	}

	next, ok := nextLockFreeState(&lockFreeState{tokens: justUnderOne, last: now}, now, 1, 1, 1)
	if !ok {
		t.Fatalf("nextLockFreeState with tokens = %v denied AllowN(1): a client one float64 ULP short of a whole token must be admitted (admitEpsilon missing from the spend comparison?)", justUnderOne)
	}
	if next == nil {
		t.Fatal("nextLockFreeState admitted but returned a nil snapshot")
	}
	if !next.last.Equal(now) {
		t.Fatalf("admitted snapshot carries last = %v, want %v (the admitting call must commit its own timestamp)", next.last, now)
	}

	// Well outside the epsilon: 1e-6 short of a token is a real shortfall.
	tooFewTokens := 1 - 1e-6
	if _, ok := nextLockFreeState(&lockFreeState{tokens: tooFewTokens, last: now}, now, 1, 1, 1); ok {
		t.Fatalf("nextLockFreeState with tokens = %v admitted AllowN(1): admitEpsilon (1e-9) must absorb float residue only, not hand out a token that has not accrued", tooFewTokens)
	}
}

// TestLockFreeTokenBucket_SteadyStateNonBinaryExactStep mirrors the baseline's
// steady-state case: a client polling at exactly the configured rate, with a
// step whose token credit (0.1) has no exact float64 representation, must land
// exactly the configured number of admissions — no under-admission over 10000
// steps.
//
// Scope, honestly: for this implementation the case does not exercise
// admitEpsilon and must not be read as its regression test. Denied calls do
// not CAS, so the timestamp advances only on admissions; elapsed between two
// admissions is therefore always exactly 1s of whole fakeClock durations, the
// refill is credited in one shot instead of as ten 0.1 additions, and no
// residue accrues at all — the test stays green with admitEpsilon removed
// (verified by mutation). What it does pin is that this one-shot crediting
// never drops or double-counts a pending refill across the denied calls in
// between. The epsilon itself is pinned by
// TestNextLockFreeState_AdmitEpsilonCoversFloatResidue above.
func TestLockFreeTokenBucket_SteadyStateNonBinaryExactStep(t *testing.T) {
	const (
		steps = 10000                  // 1000s of simulated time
		step  = 100 * time.Millisecond // rate*step == 0.1 tokens, not binary-exact
		want  = steps / 10             // one token accrues every ten steps
	)

	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLockFreeTokenBucket(1, 1, clk)

	allowed := 0
	for i := 0; i < steps; i++ {
		clk.Advance(step)
		if b.Allow() {
			allowed++
		}
	}

	// The bucket starts full, but that initial token is not an extra admission:
	// the first step's refill is clamped away by the capacity cap, so the token
	// it would have accrued is simply spent on call 1 instead of call 11.
	if allowed != want {
		t.Fatalf("admitted %d requests over %v, want exactly %d (client calls at exactly the refill rate); float drift in the spend comparison?",
			allowed, time.Duration(steps)*step, want)
	}
}

// TestLockFreeTokenBucket_ConcurrentNoOverAdmit is the central stress test of
// Этап 4: many goroutines hammer one bucket with frozen time (refill = 0), so
// the CAS loop is under maximal contention and the total number of admitted
// requests must come out at exactly the initial capacity. More than capacity
// means a race or a lost update in the CAS coordination; fewer means tokens
// were destroyed without being granted, which with frozen time is a defect
// too. Run with -race, -count>1 and GOMAXPROCS>1.
//
// The admission counter is a shared atomic.Int64 bumped on every single
// admission, deliberately: contention on shared state is the subject under
// test, and accumulating in a goroutine-local counter would lift exactly the
// pressure this test exists to apply (SKILL §5, PR #7).
//
// Two capacities, because the capacity decides which branch of the loop is
// actually contended. With capacity far below the call count, the bucket is
// empty after the first few hundred calls and >99% of the calls take the
// CAS-free deny path — a sharp over-admit detector, but barely a stress test
// of the CAS itself. With capacity at half the call count, roughly half the
// calls commit a CAS, so lost CAS races and retries are exercised in bulk. The
// exact-equality invariant holds either way; it only needs calls > capacity.
func TestLockFreeTokenBucket_ConcurrentNoOverAdmit(t *testing.T) {
	const (
		goroutines = 32
		callsPer   = 500
	)

	tests := []struct {
		name     string
		capacity int
	}{
		// Mostly deny path: the token budget runs out almost immediately.
		{name: "capacity far below call count", capacity: 100},
		// Mostly CAS path: about half of the 16000 calls commit a transition.
		{name: "capacity at half the call count", capacity: goroutines * callsPer / 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(0, 0)}
			b := NewLockFreeTokenBucket(1, tt.capacity, clk)

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

			if got := admitted.Load(); got != int64(tt.capacity) {
				t.Fatalf("admitted %d requests, want exactly %d (time frozen, no refill; over = race/lost update, under = destroyed tokens)", got, tt.capacity)
			}
		})
	}
}

// TestLockFreeTokenBucket_ConcurrentWithRefill races the refill path itself,
// which the frozen-time stress test above cannot: with time frozen, elapsed is
// always zero and the timestamp-advancing CAS transitions are never exercised
// under concurrency. Here the clock moves while workers hammer Allow, so
// admissions race against concurrent refill recomputations.
//
// The assertion is exact equality with capacity+refill, which needs every
// accrued token to be offered to the workers, not just some of them. A looser
// lower bound would be satisfied by the initially full bucket alone and would
// stay green even if a defect destroyed the entire accrued refill — the one
// branch this test exists for. Three properties make the exact count
// deterministic despite the nondeterministic interleaving:
//
//   - workers run until the driver stops them, rather than a fixed number of
//     calls, so they cannot finish before the last tick is credited;
//   - the driver does not start ticking until the initially full bucket has
//     been drained, so no early tick can be clamped away at capacity;
//   - the whole refill (20 tokens) is far below capacity (50), so no later
//     tick can be clamped either, however long the workers stall.
//
// Under those, the total token budget is exactly capacity + rate*elapsed and
// every token is claimed. The real-time deadline is only a hang guard: on
// expiry the test proceeds to the assertion and fails there with a count.
func TestLockFreeTokenBucket_ConcurrentWithRefill(t *testing.T) {
	const (
		capacity   = 50
		rate       = 100.0 // tokens per second
		goroutines = 16
		ticks      = 20
		tick       = 10 * time.Millisecond
	)

	elapsed := time.Duration(ticks) * tick
	refill := int64(rate * elapsed.Seconds()) // 20 tokens; exact in float64
	total := int64(capacity) + refill

	clk := &fakeClock{now: time.Unix(0, 0)}
	b := NewLockFreeTokenBucket(rate, capacity, clk)

	var admitted atomic.Int64
	stop := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if b.Allow() {
					admitted.Add(1)
				}
			}
		}()
	}

	// Spin until the workers have claimed want tokens. Returns false if the
	// hang guard fires first; the caller carries on to the assertion. The
	// budget is per call and generous: the workers only need to land a few
	// dozen admissions, so anything near it means they are not running at all.
	waitFor := func(want int64) bool {
		deadline := time.Now().Add(5 * time.Second)
		for admitted.Load() < want {
			if time.Now().After(deadline) {
				return false
			}
			runtime.Gosched()
		}
		return true
	}

	drained := waitFor(capacity)
	// Drive the clock from here, interleaved with the workers, so the refill
	// arithmetic runs concurrently with admission decisions.
	for i := 0; i < ticks; i++ {
		clk.Advance(tick)
		runtime.Gosched()
	}
	claimed := waitFor(total)

	close(stop)
	wg.Wait()

	if got := admitted.Load(); got != total {
		t.Fatalf("admitted %d requests, want exactly %d (capacity %d plus %v of refill at %v/s; drained-before-ticks=%v, refill-claimed=%v; over = race/lost update, under = refill destroyed or never offered)",
			got, total, capacity, elapsed, rate, drained, claimed)
	}
}
