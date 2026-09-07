package limiter

import (
	"math"
	"testing"
	"time"
)

// TestAdmitEpsilon_ExactAtUnitBatch is the backward-compatibility anchor for
// the move from an absolute constant to a relative one: every Allow() path in
// the package compares against a limit of 1, and multiplying 1e-9 by an exact
// 1.0 is exact, so those comparisons must keep producing bit-for-bit the same
// verdicts they did before. Hence ==, not a tolerance.
func TestAdmitEpsilon_ExactAtUnitBatch(t *testing.T) {
	if got := admitEpsilon(1); got != 1e-9 {
		t.Fatalf("admitEpsilon(1) = %v, want exactly 1e-9 (the historical absolute constant; every AllowN(1) verdict must stay identical)", got)
	}
}

// TestAdmitEpsilon_LiftsFloat64Ceiling is the regression test for the defect
// the relative form exists to remove. With the old absolute 1e-9, adding the
// epsilon to a limit of 2^24 or more was a no-op — float64 spacing there
// (3.7e-9 above 2^24) swallows it whole — so the epsilon quietly stopped
// working exactly where limits get large, which is the case as soon as a
// limiter meters volume rather than request counts (RESEARCH §2.2).
func TestAdmitEpsilon_LiftsFloat64Ceiling(t *testing.T) {
	for _, limit := range []float64{1 << 24, 1 << 30, 1 << 40} {
		if got := limit + admitEpsilon(limit); got <= limit {
			t.Errorf("limit %v + admitEpsilon(%v) = %v, want > %v (the slack must not vanish into the ULP of the limit)", limit, limit, got, limit)
		}
	}

	// And the old constant really did vanish there — pin the premise, so the
	// test above cannot be read as testing nothing.
	const oldAbsolute = 1e-9
	if got := float64(1<<24) + oldAbsolute; got != float64(1<<24) {
		t.Fatalf("test premise broken: 2^24 + 1e-9 = %v, expected it to round back to 2^24", got)
	}
}

// TestNextLockFreeState_AdmitEpsilonScalesWithBatch checks the relative epsilon
// through a real admission decision rather than through arithmetic on its own,
// using the pure transition function of LockFreeTokenBucket (same package, no
// black-box path reaches this state — see the note on
// TestNextLockFreeState_AdmitEpsilonCoversFloatResidue).
//
// The two cases bracket the slack at a large limit, mirroring the unit-batch
// bracket in lockfree_tokenbucket_test.go: a shortfall of a couple of ULPs must
// be absorbed (it is not, with the old absolute constant — this case is what
// fails on it, which is precisely the behavioural change being made), and a
// shortfall six orders of magnitude larger must still be denied, so the wider
// slack has not turned into a free token.
func TestNextLockFreeState_AdmitEpsilonScalesWithBatch(t *testing.T) {
	now := time.Unix(0, 0)

	const (
		batch    = 1 << 24 // where the absolute epsilon degenerated
		capacity = 1 << 25 // above the batch, so the refill clamp is not in play
		// oldAbsolute is what admitEpsilon used to be, unconditionally.
		oldAbsolute = 1e-9
	)

	// Two ULPs below the batch size — the order of residue a float64 accrual
	// leaves at this magnitude (drift is ~1e-16 relative, and one ULP here is
	// already 1.9e-9). One ULP is not enough to discriminate: 1e-9 exceeds half
	// the spacing below 2^24, so the old constant still rounded that case up to
	// the batch. Two ULPs is the first shortfall it cannot bridge.
	justUnderBatch := math.Nextafter(math.Nextafter(batch, 0), 0)
	if justUnderBatch+oldAbsolute >= batch {
		t.Fatalf("test setup: tokens = %v is bridged by the old absolute epsilon, so this case would pass without the relative form and proves nothing", justUnderBatch)
	}

	if _, ok := nextLockFreeState(&lockFreeState{tokens: justUnderBatch, last: now}, now, 1, capacity, batch); !ok {
		t.Fatalf("nextLockFreeState with tokens = %v denied AllowN(%d): a shortfall of %v — two float64 ULPs — must be absorbed, and only a slack that scales with the limit can do that at this magnitude", justUnderBatch, batch, float64(batch)-justUnderBatch)
	}

	// A shortfall of 1e-6 relative is ~16.8 tokens — a thousand times the
	// 0.0168 slack the relative epsilon grants at this limit.
	tooFewTokens := float64(batch) * (1 - 1e-6)
	if _, ok := nextLockFreeState(&lockFreeState{tokens: tooFewTokens, last: now}, now, 1, capacity, batch); ok {
		t.Fatalf("nextLockFreeState with tokens = %v admitted AllowN(%d): the relative epsilon must absorb float residue only, not hand out %v tokens that have not accrued", tooFewTokens, batch, float64(batch)-tooFewTokens)
	}
}
