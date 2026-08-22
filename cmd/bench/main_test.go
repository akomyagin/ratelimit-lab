package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akomyagin/ratelimit-lab/internal/limiter"
)

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil) = error %v, want success", err)
	}

	if !reflect.DeepEqual(cfg.algos, allAlgos) {
		t.Errorf("algos = %v, want %v (bare run must produce the comparison table)", cfg.algos, allAlgos)
	}
	if cfg.format != formatText {
		t.Errorf("format = %q, want %q", cfg.format, formatText)
	}
	if cfg.percentiles {
		t.Error("percentiles = true, want false by default (per-call timing is opt-in)")
	}
	if cfg.goroutines < 1 {
		t.Errorf("goroutines = %d, want >= 1", cfg.goroutines)
	}
	if cfg.rate <= 0 {
		t.Errorf("rate = %g, want > 0", cfg.rate)
	}
	if cfg.capacity < 1 {
		t.Errorf("capacity = %d, want >= 1", cfg.capacity)
	}
	if cfg.duration <= 0 {
		t.Errorf("duration = %s, want > 0", cfg.duration)
	}
}

func TestParseConfigSingleAlgo(t *testing.T) {
	for _, algo := range allAlgos {
		cfg, err := parseConfig([]string{"-algo=" + algo})
		if err != nil {
			t.Fatalf("parseConfig(-algo=%s) = error %v, want success", algo, err)
		}
		if want := []string{algo}; !reflect.DeepEqual(cfg.algos, want) {
			t.Errorf("-algo=%s: algos = %v, want %v", algo, cfg.algos, want)
		}
	}
}

func TestParseConfigOverrides(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-goroutines=3", "-rate=250.5", "-capacity=7",
		"-duration=50ms", "-format=markdown", "-percentiles",
	})
	if err != nil {
		t.Fatalf("parseConfig = error %v, want success", err)
	}

	if cfg.goroutines != 3 {
		t.Errorf("goroutines = %d, want 3", cfg.goroutines)
	}
	if cfg.rate != 250.5 {
		t.Errorf("rate = %g, want 250.5", cfg.rate)
	}
	if cfg.capacity != 7 {
		t.Errorf("capacity = %d, want 7", cfg.capacity)
	}
	if cfg.duration != 50*time.Millisecond {
		t.Errorf("duration = %s, want 50ms", cfg.duration)
	}
	if cfg.format != formatMarkdown {
		t.Errorf("format = %q, want %q", cfg.format, formatMarkdown)
	}
	if !cfg.percentiles {
		t.Error("percentiles = false, want true")
	}
}

// TestParseConfigValidation walks every rejection branch: bad input must come
// back as an error the caller can print, never as a panic.
func TestParseConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown algo", []string{"-algo=bogus"}},
		{"unknown format", []string{"-format=bogus"}},
		{"zero goroutines", []string{"-goroutines=0"}},
		{"negative goroutines", []string{"-goroutines=-1"}},
		{"zero rate", []string{"-rate=0"}},
		{"negative rate", []string{"-rate=-1"}},
		{"zero capacity", []string{"-capacity=0"}},
		{"zero duration", []string{"-duration=0"}},
		{"negative duration", []string{"-duration=-1s"}},
		{"undefined flag", []string{"-nosuchflag"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseConfig(tt.args); err == nil {
				t.Fatalf("parseConfig(%v) = nil error, want rejection", tt.args)
			}
		})
	}
}

// TestParseConfigSlidingRateValidation pins the rate constraints that only
// apply when -algo includes sliding: NewSlidingWindow truncates -rate to int,
// so a sub-1 rate silently denies everything and a rate beyond MaxInt64 hits
// an unspecified float-to-int conversion.
func TestParseConfigSlidingRateValidation(t *testing.T) {
	if _, err := parseConfig([]string{"-algo=sliding", "-rate=0.5"}); err == nil {
		t.Error("parseConfig(-algo=sliding -rate=0.5) = nil error, want rejection")
	}

	if _, err := parseConfig([]string{"-algo=lockfree", "-rate=0.5"}); err != nil {
		t.Errorf("parseConfig(-algo=lockfree -rate=0.5) = error %v, want success (sliding-only constraint)", err)
	}

	if _, err := parseConfig([]string{"-algo=all", "-rate=0.5"}); err == nil {
		t.Error("parseConfig(-algo=all -rate=0.5) = nil error, want rejection (all includes sliding)")
	}

	if _, err := parseConfig([]string{"-algo=sliding", "-rate=1e19"}); err == nil {
		t.Error("parseConfig(-algo=sliding -rate=1e19) = nil error, want rejection (exceeds MaxInt64)")
	}

	if _, err := parseConfig([]string{"-algo=sliding", "-rate=1"}); err != nil {
		t.Errorf("parseConfig(-algo=sliding -rate=1) = error %v, want success (boundary is valid)", err)
	}
}

func TestMakeLimiter(t *testing.T) {
	cfg, err := parseConfig([]string{"-rate=1000", "-capacity=5"})
	if err != nil {
		t.Fatalf("parseConfig = error %v", err)
	}

	tests := []struct {
		algo string
		want any
	}{
		{algoToken, (*limiter.TokenBucket)(nil)},
		{algoSliding, (*limiter.SlidingWindow)(nil)},
		{algoLeaky, (*limiter.LeakyBucket)(nil)},
		{algoLockFree, (*limiter.LockFreeTokenBucket)(nil)},
	}

	for _, tt := range tests {
		t.Run(tt.algo, func(t *testing.T) {
			lim, err := makeLimiter(tt.algo, cfg)
			if err != nil {
				t.Fatalf("makeLimiter(%q) = error %v, want success", tt.algo, err)
			}
			if lim == nil {
				t.Fatalf("makeLimiter(%q) = nil limiter", tt.algo)
			}
			if got, want := reflect.TypeOf(lim), reflect.TypeOf(tt.want); got != want {
				t.Fatalf("makeLimiter(%q) = %v, want %v", tt.algo, got, want)
			}
			// A freshly built limiter with capacity 5 / limit 1000 must admit
			// its first request; this also proves the mapping did not hand a
			// degenerate parameter to the constructor.
			if !lim.Allow() {
				t.Errorf("makeLimiter(%q): first Allow = false, want true", tt.algo)
			}
		})
	}
}

func TestMakeLimiterUnknown(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig = error %v", err)
	}
	if _, err := makeLimiter("bogus", cfg); err == nil {
		t.Fatal("makeLimiter(\"bogus\") = nil error, want rejection")
	}
}

// TestRunOneSmoke is a liveness check on the driver, not a measurement: it
// asserts only invariants that hold on any machine, because timing assertions
// here would flake without telling us anything the real run does not.
func TestRunOneSmoke(t *testing.T) {
	cfg := config{
		algos:      []string{algoToken},
		goroutines: 2,
		rate:       1e9,
		capacity:   1 << 30,
		duration:   20 * time.Millisecond,
		format:     formatText,
	}

	lim, err := makeLimiter(algoToken, cfg)
	if err != nil {
		t.Fatalf("makeLimiter = error %v", err)
	}

	r := runOne(algoToken, lim, cfg)

	if r.algo != algoToken {
		t.Errorf("algo = %q, want %q", r.algo, algoToken)
	}
	if r.calls == 0 {
		t.Error("calls = 0, want > 0 (the driver did no work)")
	}
	if r.allowed == 0 {
		t.Error("allowed = 0, want > 0 (bucket sized to never run dry)")
	}
	if r.allowed > r.calls {
		t.Errorf("allowed = %d > calls = %d", r.allowed, r.calls)
	}
	if r.workers != cfg.goroutines {
		t.Errorf("workers = %d, want %d", r.workers, cfg.goroutines)
	}
	if r.elapsed < cfg.duration {
		t.Errorf("elapsed = %s, want at least the configured %s", r.elapsed, cfg.duration)
	}
	if r.hist != nil {
		t.Error("hist != nil with percentiles disabled")
	}
}

// TestRunWiring exercises the whole binary short of main: flags in, table out.
// Durations are kept tiny — this checks that the pieces are connected, the
// measurement itself is only meaningful at the default settings.
func TestRunWiring(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "markdown, single algo",
			args: []string{"-algo=lockfree", "-goroutines=2", "-duration=20ms", "-format=markdown"},
			want: []string{"| algo |", "|---|", "| lockfree |"},
		},
		{
			name: "text, all algos",
			args: []string{"-goroutines=2", "-duration=20ms"},
			want: []string{algoToken, algoSliding, algoLeaky, algoLockFree, "allocs/op (approx)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := run(tt.args, &buf); err != nil {
				t.Fatalf("run(%v) = error %v, want success", tt.args, err)
			}
			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	var buf bytes.Buffer
	if err := run([]string{"-algo=bogus"}, &buf); err == nil {
		t.Fatal("run(-algo=bogus) = nil error, want rejection")
	}
	if buf.Len() != 0 {
		t.Errorf("rejected run still wrote to stdout: %q", buf.String())
	}
}

func TestRunOnePercentilesPopulatesHistogram(t *testing.T) {
	cfg := config{
		algos:       []string{algoLockFree},
		goroutines:  2,
		rate:        1e9,
		capacity:    1 << 30,
		duration:    20 * time.Millisecond,
		format:      formatText,
		percentiles: true,
	}

	lim, err := makeLimiter(algoLockFree, cfg)
	if err != nil {
		t.Fatalf("makeLimiter = error %v", err)
	}

	r := runOne(algoLockFree, lim, cfg)
	if r.hist == nil {
		t.Fatal("hist = nil with -percentiles on")
	}

	var total uint64
	for _, c := range r.hist.buckets {
		total += c
	}
	if total != r.calls {
		t.Errorf("histogram holds %d samples, want one per call (%d)", total, r.calls)
	}
}
