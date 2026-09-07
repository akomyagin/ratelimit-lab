package limiter

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// fakeSinceClock is the fake that offers the SinceClock fast path. It wraps the
// shared fakeClock rather than replacing it, on purpose: clock_test.go's fake
// stays Now-only so that the fallback path keeps being exercised too, and every
// scenario below runs against both.
type fakeSinceClock struct{ fakeClock }

// Since returns the elapsed duration, derived from the same controllable now.
func (c *fakeSinceClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// atomicClockMode is one of the two time sources AtomicTokenBucket can be built
// on. Both must produce identical verdicts: the fast path is an optimization,
// never a semantic difference.
type atomicClockMode struct {
	name string
	// build returns a clock frozen at the epoch plus its advance function.
	build func() (Clock, func(time.Duration))
}

var atomicClockModes = []atomicClockMode{
	{
		name: "Now fallback",
		build: func() (Clock, func(time.Duration)) {
			c := &fakeClock{now: time.Unix(0, 0)}
			return c, c.Advance
		},
	},
	{
		name: "Since fast path",
		build: func() (Clock, func(time.Duration)) {
			c := &fakeSinceClock{fakeClock{now: time.Unix(0, 0)}}
			return c, c.Advance
		},
	},
}

// TestAtomicTokenBucket_Scenarios is the scenario table of
// TestLockFreeTokenBucket_Scenarios, case for case: this limiter changes the
// state representation, not the policy, so it must pass the baseline's own
// correctness suite unchanged. Every rate here has an integral emission
// interval, which is where the two schemes are required to agree exactly.
//
// Each case runs twice, once per clock mode: the SinceClock fast path is
// selected at construction time, so without both runs half the time source
// logic would go untested.
func TestAtomicTokenBucket_Scenarios(t *testing.T) {
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
			// The case above at capacity 2: it pins that the half-second of
			// credit pending across a denied call is neither lost nor doubled
			// once the bucket can hold more than one token. Here that is a
			// statement about the clamp, since the pending credit is never
			// stored — it is the gap between vt and now.
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

	for _, mode := range atomicClockModes {
		t.Run(mode.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					clk, advance := mode.build()
					b := NewAtomicTokenBucket(tt.rate, tt.capacity, clk)
					for i, s := range tt.steps {
						if s.advance > 0 {
							advance(s.advance)
						}
						if got := b.AllowN(s.n); got != s.want {
							t.Fatalf("step %d: AllowN(%d) = %v, want %v", i, s.n, got, s.want)
						}
					}
				})
			}
		})
	}
}

// TestAtomicTokenBucket_AllowNNonPositiveKeepsPendingCredit pins the stricter
// part of the port contract, mirroring both neighbours: AllowN(n <= 0) must not
// touch state at all. Here "state" is the virtual zero time, so the half second
// of credit pending before the non-positive calls must still be creditable,
// exactly once, to the next real call.
func TestAtomicTokenBucket_AllowNNonPositiveKeepsPendingCredit(t *testing.T) {
	clk := &fakeSinceClock{fakeClock{now: time.Unix(0, 0)}}
	b := NewAtomicTokenBucket(1, 1, clk)

	if !b.Allow() {
		t.Fatal("initial Allow() = false, want true (bucket starts full)")
	}

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

// TestAtomicTokenBucket_SteadyStateNonBinaryExactStep is the case that made the
// project add admitEpsilon in the first place: a client polling at exactly the
// configured rate, on a step whose token credit (0.1) has no exact float64
// representation, must land exactly the configured number of admissions.
//
// Scope, stated as plainly as on the neighbours: this is not an epsilon
// regression test, it is the demonstration that the epsilon is structurally
// unnecessary here. The step is 1e8 ns and the emission interval is 1e9 ns, both
// exact integers, and every operation between them is integer arithmetic, so
// there is no residue to accrue and nothing for a slack to absorb — removing
// admitEpsilon from the package entirely would leave this test green. That is
// the claim: 1000 admissions from a scheme that carries no fudge factor.
func TestAtomicTokenBucket_SteadyStateNonBinaryExactStep(t *testing.T) {
	const (
		steps = 10000                  // 1000s of simulated time
		step  = 100 * time.Millisecond // rate*step == 0.1 tokens, not binary-exact
		want  = steps / 10             // one token accrues every ten steps
	)

	clk := &fakeSinceClock{fakeClock{now: time.Unix(0, 0)}}
	b := NewAtomicTokenBucket(1, 1, clk)

	allowed := 0
	for i := 0; i < steps; i++ {
		clk.Advance(step)
		if b.Allow() {
			allowed++
		}
	}

	// The bucket starts full, but that initial token is not an extra admission:
	// the first step's credit is clamped away by the capacity cap, so the token
	// it would have accrued is simply spent on call 1 instead of call 11.
	if allowed != want {
		t.Fatalf("admitted %d requests over %v, want exactly %d (client calls at exactly the refill rate)",
			allowed, time.Duration(steps)*step, want)
	}
}

// TestNextAtomicState_Transitions drives the pure transition function directly,
// over the branches a black-box call sequence cannot reach reliably: the
// capacity clamp, the inclusive admission boundary from both sides, and the
// negative-availability case that only arises under concurrency, when another
// caller with a later clock reading has already pushed vt past our own now.
//
// All values are nanoseconds. The monotonicity of vt on admission — the reason
// ABA cannot occur in this limiter — is asserted for every admitting case.
func TestNextAtomicState_Transitions(t *testing.T) {
	const (
		second = int64(time.Second)
		capT   = 10 * second // ten emission intervals of one second
		need   = second      // one unit
	)

	tests := []struct {
		name     string
		vt       int64
		nowNs    int64
		capT     int64
		need     int64
		wantOK   bool
		wantNext int64
	}{
		{
			name:     "idle longer than the span is clamped to one full bucket",
			vt:       0,
			nowNs:    5000 * second, // 500x the full-refill time
			capT:     capT,
			need:     need,
			wantOK:   true,
			wantNext: 4991 * second, // floor 4990s + one interval, not 1s
		},
		{
			name:     "availability exactly equal to the batch is admitted",
			vt:       0,
			nowNs:    second,
			capT:     capT,
			need:     need,
			wantOK:   true,
			wantNext: second,
		},
		{
			name:   "availability one nanosecond short is denied",
			vt:     0,
			nowNs:  second - 1,
			capT:   capT,
			need:   need,
			wantOK: false,
		},
		{
			name:     "a whole batch is charged in one step",
			vt:       0,
			nowNs:    10 * second,
			capT:     capT,
			need:     4 * second,
			wantOK:   true,
			wantNext: 4 * second,
		},
		{
			name:   "no availability at all is denied",
			vt:     second,
			nowNs:  second,
			capT:   capT,
			need:   need,
			wantOK: false,
		},
		{
			name:   "vt ahead of now (concurrent caller with a later clock) is denied",
			vt:     2 * second,
			nowNs:  second,
			capT:   capT,
			need:   need,
			wantOK: false,
		},
		{
			name:   "a degenerate zero span never admits",
			vt:     0,
			nowNs:  5000 * second,
			capT:   0,
			need:   need,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, ok := nextAtomicState(tt.vt, tt.nowNs, tt.capT, tt.need)
			if ok != tt.wantOK {
				t.Fatalf("nextAtomicState(vt=%d, now=%d, capT=%d, need=%d) ok = %v, want %v", tt.vt, tt.nowNs, tt.capT, tt.need, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if next != tt.wantNext {
				t.Fatalf("next = %d, want %d", next, tt.wantNext)
			}
			if next <= tt.vt {
				t.Fatalf("next = %d is not greater than vt = %d: an admission must move the virtual zero time strictly forward, which is what rules out ABA on the CAS", next, tt.vt)
			}
		})
	}
}

// TestAtomicTokenBucket_ConcurrentNoOverAdmit is the central stress test: many
// goroutines hammer one bucket with time frozen (no refill), so the CAS loop
// runs under maximal contention and the total number of admissions must come
// out at exactly the initial capacity. More than capacity means a race or a
// lost update; fewer means credit was destroyed without being granted, which
// with frozen time is a defect too. Run with -race, -count>1 and GOMAXPROCS>1.
//
// The counter is a shared atomic.Int64 bumped on every single admission,
// deliberately: contention on shared state is the subject under test, and
// accumulating in a goroutine-local counter would lift exactly the pressure the
// test exists to apply (SKILL §5, PR #7).
//
// Two capacities, because the capacity decides which branch is contended. Far
// below the call count, the bucket empties almost at once and nearly every call
// takes the CAS-free deny path — a sharp over-admit detector, but barely a test
// of the CAS. At half the call count, about half the calls commit a CAS, so
// lost races and retries are exercised in bulk.
func TestAtomicTokenBucket_ConcurrentNoOverAdmit(t *testing.T) {
	const (
		goroutines = 32
		callsPer   = 500
	)

	tests := []struct {
		name     string
		capacity int
	}{
		{name: "capacity far below call count", capacity: 100},
		{name: "capacity at half the call count", capacity: goroutines * callsPer / 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := &fakeSinceClock{fakeClock{now: time.Unix(0, 0)}}
			b := NewAtomicTokenBucket(1, tt.capacity, clk)

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
				t.Fatalf("admitted %d requests, want exactly %d (time frozen, no refill; over = race/lost update, under = destroyed credit)", got, tt.capacity)
			}
		})
	}
}

// TestAtomicTokenBucket_ConcurrentWithRefill races the refill path itself, which
// the frozen-time test above cannot: with time frozen, vt never has to be
// clamped and admissions never race against a moving now. Here the clock moves
// while workers hammer Allow.
//
// The assertion is exact equality with capacity+refill, which needs every
// accrued unit to be offered to the workers, not just some. A looser lower
// bound would be satisfied by the initially full bucket alone and would stay
// green even if a defect destroyed the entire refill. Three properties make the
// exact count deterministic despite the nondeterministic interleaving:
//
//   - workers run until the driver stops them, rather than a fixed number of
//     calls, so they cannot finish before the last tick is credited;
//   - the driver does not tick until the initially full bucket is drained, so
//     no early tick is clamped away at capacity;
//   - the whole refill (20 units) is far below capacity (50), so no later tick
//     is clamped either, however long the workers stall.
//
// rate 100 gives an emission interval of exactly 1e7 ns, so the quantization
// documented on the constructor is zero here and the count is not approximate.
// The real-time deadline is only a hang guard: on expiry the test proceeds to
// the assertion and fails there with a count.
func TestAtomicTokenBucket_ConcurrentWithRefill(t *testing.T) {
	const (
		capacity   = 50
		rate       = 100.0 // tokens per second; emission interval 1e7 ns exactly
		goroutines = 16
		ticks      = 20
		tick       = 10 * time.Millisecond
	)

	elapsed := time.Duration(ticks) * tick
	refill := int64(rate * elapsed.Seconds()) // 20 tokens
	total := int64(capacity) + refill

	clk := &fakeSinceClock{fakeClock{now: time.Unix(0, 0)}}
	b := NewAtomicTokenBucket(rate, capacity, clk)

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

	// Spin until the workers have claimed want units. Returns false if the hang
	// guard fires first; the caller carries on to the assertion.
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

// TestNewAtomicTokenBucket_Validation walks the constructor's rejection
// branches. Unlike TokenBucket, this constructor divides by rate and packs the
// result into int64, so parameters that make that meaningless are programming
// errors reported at construction — the same rule NewSlidingWindow follows.
// A non-positive capacity is explicitly not one of them.
func TestNewAtomicTokenBucket_Validation(t *testing.T) {
	inf := math.Inf(1)

	panics := []struct {
		name     string
		rate     float64
		capacity int
	}{
		{"zero rate", 0, 10},
		{"negative rate", -1, 10},
		{"NaN rate", math.NaN(), 10},
		{"infinite rate", inf, 10},
		{"rate needing a sub-nanosecond interval", 3e9, 10},
		{"rate needing an interval past int64", 1e-10, 10},
		// One per hour with a burst of three million: capacity*interval
		// overflows. This is the trap that bites real configurations.
		{"capacity overflowing the bucket span", 1.0 / 3600, 3_000_000},
	}

	for _, tt := range panics {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewAtomicTokenBucket(%g, %d, clk) returned normally, want panic", tt.rate, tt.capacity)
				}
			}()
			NewAtomicTokenBucket(tt.rate, tt.capacity, &fakeClock{now: time.Unix(0, 0)})
		})
	}

	// Boundary that must be accepted: rate 2e9 rounds to an interval of exactly
	// 1 ns, the finest this scheme represents.
	t.Run("rate at the resolution boundary is accepted", func(t *testing.T) {
		b := NewAtomicTokenBucket(2e9, 1, &fakeClock{now: time.Unix(0, 0)})
		if !b.Allow() {
			t.Error("first Allow() = false, want true (bucket starts full)")
		}
	})

	for _, capacity := range []int{0, -1} {
		t.Run("degenerate capacity is accepted and denies everything", func(t *testing.T) {
			b := NewAtomicTokenBucket(1, capacity, &fakeClock{now: time.Unix(0, 0)})
			if b.Allow() {
				t.Errorf("capacity %d: Allow() = true, want false (nothing can ever fit)", capacity)
			}
			if b.AllowN(1000) {
				t.Errorf("capacity %d: AllowN(1000) = true, want false", capacity)
			}
			if !b.AllowN(0) {
				t.Errorf("capacity %d: AllowN(0) = false, want true (port contract holds regardless)", capacity)
			}
		})
	}
}

// TestNewAtomicTokenBucket_RateQuantization pins the calibration documented on
// the constructor: the rate is stored as an integer emission interval, so what
// gets enforced is 1e9/round(1e9/rate). White-box on purpose — the quantization
// is invisible through the port at these magnitudes, and it is exactly the kind
// of documented approximation that a later "cleanup" would silently change.
func TestNewAtomicTokenBucket_RateQuantization(t *testing.T) {
	tests := []struct {
		rate float64
		want int64
	}{
		{rate: 1e9, want: 1},             // the benchmark default: exact
		{rate: 3, want: 333333333},       // 333333333.33 rounds down
		{rate: 0.5, want: 2_000_000_000}, // one per two seconds
		{rate: 1.5e9, want: 1},           // 0.667 rounds up to the 1 ns floor
	}

	for _, tt := range tests {
		b := NewAtomicTokenBucket(tt.rate, 1, &fakeClock{now: time.Unix(0, 0)})
		if b.nanosPerToken != tt.want {
			t.Errorf("rate %g: emission interval = %d ns, want %d", tt.rate, b.nanosPerToken, tt.want)
		}
	}
}

// TestAtomicTokenBucket_ClockFastPathSelection pins that the SinceClock
// extension is detected once, at construction, and that production actually
// gets it — an optional interface nobody satisfies would be dead weight in the
// hot path's branch.
func TestAtomicTokenBucket_ClockFastPathSelection(t *testing.T) {
	fast := NewAtomicTokenBucket(1, 1, &fakeSinceClock{fakeClock{now: time.Unix(0, 0)}})
	if fast.sinceClk == nil {
		t.Error("a Clock implementing SinceClock was not detected: the hot path would take the slower Now() route")
	}

	slow := NewAtomicTokenBucket(1, 1, &fakeClock{now: time.Unix(0, 0)})
	if slow.sinceClk != nil {
		t.Error("a Now-only Clock was taken for a SinceClock")
	}

	if _, ok := SystemClock.(SinceClock); !ok {
		t.Error("SystemClock does not implement SinceClock: production would silently run on the degraded clock path")
	}
}

// TestAtomicTokenBucket_FieldPaddingLayout pins the memory layout, which is a
// measured part of the design rather than incidental: the read-only fields must
// stay two full cache lines away from the CAS target, so that the writers
// invalidating vt's line do not also invalidate them (128, not 64 — the
// adjacent-line prefetcher pairs lines, and 64 measured worse than nothing;
// RESEARCH §1.3). Without this test, a future tidy-up of the struct would drop
// the padding and cost several times the throughput with every test still
// green.
//
// The offset of nanosPerToken is checked for exact equality with 128, not just
// >= 128: a >= check alone would still pass if a future edit slipped a second
// mutable field in between vt and the padding array (e.g. at offset 8), since
// that field would simply push nanosPerToken further out, never closer to vt.
// Such a field would defeat the whole design — it would share vt's cache line
// and get invalidated on every CAS retry — while still satisfying "at least
// 128". Pinning the exact offset, plus vt's own size accounting for the first
// 8 bytes, rules that out: the only way to land nanosPerToken at exactly 128
// is for the gap to be vt (8 bytes) followed immediately by the 120-byte pad
// and nothing else.
func TestAtomicTokenBucket_FieldPaddingLayout(t *testing.T) {
	var b AtomicTokenBucket

	if got := unsafe.Offsetof(b.vt); got != 0 {
		t.Errorf("vt is at offset %d, want 0 (the CAS target leads the struct)", got)
	}
	if got := unsafe.Sizeof(b.vt); got != 8 {
		t.Fatalf("sizeof(vt) = %d, want 8 (atomic.Int64); the offset arithmetic below assumes this", got)
	}
	if got := unsafe.Offsetof(b.nanosPerToken); got != 128 {
		t.Errorf("the first read-only field is at offset %d, want exactly 128: vt (8 bytes) followed by the 120-byte pad and nothing else — a value > 128 would mean an extra field crept in after the pad, and < 128 would mean the pad shrank or a field was inserted before offset 128", got)
	}
}
