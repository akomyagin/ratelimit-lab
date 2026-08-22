package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLatencyHistEmptyQuantile(t *testing.T) {
	var h latencyHist
	if got := h.quantile(0.5); got != 0 {
		t.Errorf("empty quantile(0.5) = %s, want 0 (must not panic or invent a value)", got)
	}
	if got := h.quantile(0.999); got != 0 {
		t.Errorf("empty quantile(0.999) = %s, want 0", got)
	}
}

// TestLatencyHistSingleBucket pins the resolution contract: with every sample
// inside one power-of-two bucket, any quantile is that bucket's upper bound.
func TestLatencyHistSingleBucket(t *testing.T) {
	var h latencyHist
	// 100..127ns all have bit length 7, so they share bucket 7 = [64,127]ns.
	for d := 100; d < 128; d++ {
		h.observe(time.Duration(d))
	}

	want := 127 * time.Nanosecond
	for _, q := range []float64{0.0, 0.5, 0.99, 0.999, 1.0} {
		if got := h.quantile(q); got != want {
			t.Errorf("quantile(%g) = %s, want %s", q, got, want)
		}
	}
}

// TestLatencyHistTwoBuckets checks that a 90/10 split puts p50 in the low
// bucket and p99 in the high one.
func TestLatencyHistTwoBuckets(t *testing.T) {
	var h latencyHist
	for i := 0; i < 90; i++ {
		h.observe(100 * time.Nanosecond) // bucket 7 -> upper 127ns
	}
	for i := 0; i < 10; i++ {
		h.observe(100 * time.Microsecond) // bucket 17 -> upper 131071ns
	}

	lo := 127 * time.Nanosecond
	hi := time.Duration(1<<17 - 1)

	if got := h.quantile(0.50); got != lo {
		t.Errorf("p50 = %s, want %s (low bucket)", got, lo)
	}
	if got := h.quantile(0.99); got != hi {
		t.Errorf("p99 = %s, want %s (high bucket)", got, hi)
	}
	if got := h.quantile(0.999); got != hi {
		t.Errorf("p99.9 = %s, want %s (high bucket)", got, hi)
	}
}

func TestLatencyHistObserveClamps(t *testing.T) {
	var h latencyHist
	h.observe(0)
	h.observe(-5 * time.Second) // a clock that failed to advance
	if h.buckets[0] != 2 {
		t.Errorf("buckets[0] = %d, want 2 (zero and negative samples land in bucket 0)", h.buckets[0])
	}
	if got := h.quantile(0.5); got != 0 {
		t.Errorf("quantile(0.5) = %s, want 0", got)
	}
}

func TestLatencyHistMerge(t *testing.T) {
	var a, b latencyHist
	a.observe(100 * time.Nanosecond)
	a.observe(100 * time.Nanosecond)
	b.observe(100 * time.Nanosecond)
	b.observe(100 * time.Microsecond)

	a.merge(&b)

	if a.buckets[7] != 3 {
		t.Errorf("buckets[7] = %d, want 3", a.buckets[7])
	}
	if a.buckets[17] != 1 {
		t.Errorf("buckets[17] = %d, want 1", a.buckets[17])
	}
	if b.buckets[7] != 1 {
		t.Errorf("merge mutated the source: buckets[7] = %d, want 1", b.buckets[7])
	}

	a.merge(nil) // must not panic
}

// sampleResults builds two fake results so the renderers can be tested without
// running any load.
func sampleResults(withHist bool) []result {
	mk := func(algo string, calls, allowed, allocs uint64) result {
		r := result{
			algo:    algo,
			calls:   calls,
			allowed: allowed,
			elapsed: 2 * time.Second,
			allocs:  allocs,
			workers: 4,
		}
		if withHist {
			h := &latencyHist{}
			h.observe(100 * time.Nanosecond)
			h.observe(100 * time.Microsecond)
			r.hist = h
		}
		return r
	}
	return []result{
		mk(algoToken, 1000, 900, 0),
		mk(algoLockFree, 800, 800, 1600),
	}
}

func TestRenderMarkdownTable(t *testing.T) {
	cfg := config{algos: allAlgos, goroutines: 4, rate: 1e9, capacity: 1 << 30, duration: 2 * time.Second, format: formatMarkdown}

	var buf bytes.Buffer
	if err := renderMarkdown(&buf, sampleResults(false), cfg); err != nil {
		t.Fatalf("renderMarkdown = error %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"| algo | goroutines | calls | allowed | denied |",
		"throughput (calls/s)",
		"avg latency (ns)",
		"allocs/op (approx)",
		"|---|",
		"| token |",
		"| lockfree |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output missing %q:\n%s", want, out)
		}
	}

	if !strings.Contains(out, "goroutines=4") {
		t.Errorf("markdown output missing the run-context line:\n%s", out)
	}
	// denied = calls - allowed = 100 for the token row.
	if !strings.Contains(out, "| 100 |") {
		t.Errorf("markdown output missing the derived denied count:\n%s", out)
	}
	// allocs/op = 1600/800 = 2.00 for the lock-free row.
	if !strings.Contains(out, "| 2.00 |") {
		t.Errorf("markdown output missing the derived allocs/op:\n%s", out)
	}

	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "| ") {
			rows++
		}
	}
	if want := 1 + len(sampleResults(false)); rows != want {
		t.Errorf("markdown table has %d header+data rows, want %d", rows, want)
	}
}

func TestRenderTextTable(t *testing.T) {
	cfg := config{algos: allAlgos, goroutines: 4, rate: 1e9, capacity: 1 << 30, duration: 2 * time.Second, format: formatText}

	var buf bytes.Buffer
	if err := renderText(&buf, sampleResults(false), cfg); err != nil {
		t.Fatalf("renderText = error %v", err)
	}
	out := buf.String()

	for _, want := range []string{"algo", "goroutines", "throughput (calls/s)", "allocs/op (approx)", algoToken, algoLockFree} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "|---|") {
		t.Errorf("text output looks like markdown:\n%s", out)
	}
}

// TestRenderPercentileColumns pins the two things that must move together: the
// p50/p99/p99.9 columns and the overhead warning appear exactly when
// -percentiles is on, and never otherwise.
func TestRenderPercentileColumns(t *testing.T) {
	renderers := map[string]func(*bytes.Buffer, []result, config) error{
		"text": func(b *bytes.Buffer, rs []result, cfg config) error { return renderText(b, rs, cfg) },
		"markdown": func(b *bytes.Buffer, rs []result, cfg config) error {
			return renderMarkdown(b, rs, cfg)
		},
	}

	for name, render := range renderers {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/percentiles=%v", name, on), func(t *testing.T) {
				cfg := config{algos: allAlgos, goroutines: 4, rate: 1e9, capacity: 1 << 30, duration: 2 * time.Second, percentiles: on}
				var buf bytes.Buffer
				if err := render(&buf, sampleResults(on), cfg); err != nil {
					t.Fatalf("render = error %v", err)
				}
				out := buf.String()

				hasCols := strings.Contains(out, "p99.9")
				if hasCols != on {
					t.Errorf("percentiles=%v: p99.9 column present = %v, want %v:\n%s", on, hasCols, on, out)
				}

				hasNote := strings.Contains(out, "-percentiles adds a time.Now() pair")
				if hasNote != on {
					t.Errorf("percentiles=%v: overhead note present = %v, want %v:\n%s", on, hasNote, on, out)
				}
			})
		}
	}
}

// TestDeniedWarning pins the warning to the presence of at least one denying
// row: sampleResults(false) has a token row with denied=100, so the warning
// must appear; an all-admitted set must not print it.
func TestDeniedWarning(t *testing.T) {
	cfg := config{algos: allAlgos, goroutines: 4, rate: 1e9, capacity: 1 << 30, duration: 2 * time.Second}
	renderers := map[string]func(*bytes.Buffer, []result, config) error{
		"text": func(b *bytes.Buffer, rs []result, cfg config) error { return renderText(b, rs, cfg) },
		"markdown": func(b *bytes.Buffer, rs []result, cfg config) error {
			return renderMarkdown(b, rs, cfg)
		},
	}

	allAdmitted := []result{
		{algo: algoToken, calls: 1000, allowed: 1000, elapsed: 2 * time.Second, workers: 4},
		{algo: algoLockFree, calls: 800, allowed: 800, elapsed: 2 * time.Second, workers: 4},
	}

	for name, render := range renderers {
		t.Run(name+"/has denial", func(t *testing.T) {
			var buf bytes.Buffer
			if err := render(&buf, sampleResults(false), cfg); err != nil {
				t.Fatalf("render = error %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "warning: 1 row(s) recorded denials") {
				t.Errorf("output missing denied warning:\n%s", out)
			}
		})

		t.Run(name+"/no denial", func(t *testing.T) {
			var buf bytes.Buffer
			if err := render(&buf, allAdmitted, cfg); err != nil {
				t.Fatalf("render = error %v", err)
			}
			out := buf.String()
			if strings.Contains(out, "warning:") {
				t.Errorf("output has denied warning with no denials:\n%s", out)
			}
		})
	}
}

func TestDerive(t *testing.T) {
	r := result{
		algo:    algoToken,
		calls:   1000,
		allowed: 900,
		elapsed: 2 * time.Second,
		allocs:  2000,
		workers: 4,
	}

	d := derive(r)

	if d.denied != 100 {
		t.Errorf("denied = %d, want 100", d.denied)
	}
	if d.throughput != 500 {
		t.Errorf("throughput = %g, want 500", d.throughput)
	}
	// 2e9ns * 4 workers / 1000 calls
	if d.avgNs != 8e6 {
		t.Errorf("avgNs = %g, want 8e6", d.avgNs)
	}
	if d.allocsPerO != 2 {
		t.Errorf("allocsPerO = %g, want 2", d.allocsPerO)
	}
}

// TestDeriveZeroCalls guards the division-by-zero path: a run that produced
// nothing must render zeros rather than NaN.
func TestDeriveZeroCalls(t *testing.T) {
	d := derive(result{algo: algoToken, workers: 4})
	if d.throughput != 0 || d.avgNs != 0 || d.allocsPerO != 0 {
		t.Errorf("derive on an empty result = %+v, want all zeros", d)
	}
}
