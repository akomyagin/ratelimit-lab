// Command bench is the standalone benchmark runner for ratelimit-lab.
//
// It drives every limiter implementation under configurable concurrent load and
// prints a comparative throughput/latency report. The Go-native micro-benchmarks
// live in internal/limiter/*_bench_test.go (run with `go test -bench`); this
// binary is the human-facing harness that produces the comparison table shipped
// in the README.
//
// Running it without flags compares every algorithm in a fixed order, which
// is the point of the binary: a single column has nothing to be compared to.
// Pass -algo to measure one implementation in isolation.
//
// The orchestration is split across main.go (flags, algo-to-constructor
// mapping, wiring), runner.go (load driver and metric collection) and report.go
// (latency histogram and renderers). It all stays in package main: none of it
// is reusable outside the harness, no limiter algorithm lives here, and
// `go test ./cmd/bench` exercises package main just fine.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/akomyagin/ratelimit-lab/internal/limiter"
)

// Algorithm names accepted by -algo, and the fixed order they run in when
// "all" is requested. The order is fixed so that repeated runs produce
// diffable tables.
const (
	algoToken    = "token"
	algoSliding  = "sliding"
	algoLeaky    = "leaky"
	algoLockFree = "lockfree"
	algoAtomic   = "atomic"
	algoAll      = "all"
)

// allAlgos is the expansion of -algo=all, in run order.
var allAlgos = []string{algoToken, algoSliding, algoLeaky, algoLockFree, algoAtomic}

// config is the fully validated, already-expanded run configuration. It is what
// parseConfig produces and what every other function in the harness consumes;
// no flag parsing happens past that point.
type config struct {
	algos       []string // expanded list: one name, or every algorithm in run order
	goroutines  int
	rate        float64
	capacity    int
	duration    time.Duration
	format      string // "text" | "markdown"
	percentiles bool
}

// slidingWindowLength is the fixed trailing window used for the sliding-window
// limiter. NewSlidingWindow takes (limit, window) rather than (rate, capacity),
// so -rate is reinterpreted as "limit per this window": with a one-second
// window, limit/window matches the sustained rate of the other three
// algorithms, which keeps the comparison meaningful without touching the
// constructor's signature.
const slidingWindowLength = time.Second

// maxAtomicBucketSpanNanos mirrors the bucket-span limit that
// limiter.NewAtomicTokenBucket enforces by panicking. It is restated here, in
// float64, so that parseConfig can reject the same configurations as errors —
// see the atomic block in parseConfig.
const maxAtomicBucketSpanNanos = 1 << 62

// parseConfig parses argv-style arguments into a validated config.
//
// It uses its own FlagSet rather than flag.CommandLine so that tests can call
// it repeatedly without tripping over global state or re-registration panics.
// Invalid input is returned as an error, never a panic: the caller turns it
// into a diagnostic plus a non-zero exit code.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		algo        = fs.String("algo", algoAll, "algorithm to run: token, sliding, leaky, lockfree, atomic, or all (note: sliding ignores -capacity; it takes -rate as its per-second limit)")
		goroutines  = fs.Int("goroutines", runtime.GOMAXPROCS(0), "number of load-generating goroutines")
		rate        = fs.Float64("rate", 1e9, "refill/leak rate per second; kept far above the offered load so the bucket never runs dry")
		capacity    = fs.Int("capacity", 1<<30, "bucket capacity (unused by sliding)")
		duration    = fs.Duration("duration", 2*time.Second, "load duration per algorithm")
		format      = fs.String("format", "text", "output format: text or markdown")
		percentiles = fs.Bool("percentiles", false, "report p50/p99/p99.9 latency (adds per-call timing overhead to every reported figure)")
	)

	if err := fs.Parse(args); err != nil {
		// The FlagSet writes nowhere by default so that tests stay quiet; -h is
		// the one case where the user actually wants the usage text.
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stderr)
			fs.Usage()
		}
		return config{}, err
	}

	cfg := config{
		goroutines:  *goroutines,
		rate:        *rate,
		capacity:    *capacity,
		duration:    *duration,
		format:      *format,
		percentiles: *percentiles,
	}

	switch *algo {
	case algoAll:
		cfg.algos = append([]string(nil), allAlgos...)
	case algoToken, algoSliding, algoLeaky, algoLockFree, algoAtomic:
		cfg.algos = []string{*algo}
	default:
		return config{}, fmt.Errorf("unknown -algo %q: want one of token, sliding, leaky, lockfree, atomic, all", *algo)
	}

	switch cfg.format {
	case formatText, formatMarkdown:
	default:
		return config{}, fmt.Errorf("unknown -format %q: want text or markdown", cfg.format)
	}

	if cfg.goroutines < 1 {
		return config{}, fmt.Errorf("-goroutines must be >= 1, got %d", cfg.goroutines)
	}
	if cfg.rate <= 0 {
		return config{}, fmt.Errorf("-rate must be > 0, got %g", cfg.rate)
	}
	if cfg.capacity < 1 {
		return config{}, fmt.Errorf("-capacity must be >= 1, got %d", cfg.capacity)
	}
	if cfg.duration <= 0 {
		return config{}, fmt.Errorf("-duration must be > 0, got %s", cfg.duration)
	}
	if containsAlgo(cfg.algos, algoSliding) {
		// sliding takes -rate as an integer per-window limit (see
		// slidingWindowLength above): NewSlidingWindow(int(cfg.rate), ...)
		// truncates, so a sub-1 rate silently becomes a 0-limit window that
		// denies everything, and a rate beyond MaxInt64 hits an unspecified
		// float-to-int conversion in Go.
		if cfg.rate < 1 {
			return config{}, fmt.Errorf("-rate must be >= 1 for -algo=%s (or =all): sliding takes -rate as an integer limit per %s window, and truncating a sub-1 rate to 0 would deny everything, got %g", algoSliding, slidingWindowLength, cfg.rate)
		}
		if cfg.rate > math.MaxInt64 {
			return config{}, fmt.Errorf("-rate must be <= %d for -algo=%s (or =all): sliding converts -rate to int64, got %g", int64(math.MaxInt64), algoSliding, cfg.rate)
		}
	}
	if containsAlgo(cfg.algos, algoAtomic) {
		// NewAtomicTokenBucket panics on parameters it cannot represent: it
		// stores an integer emission interval of round(1e9/rate) ns and a
		// bucket span of capacity times that. Both bounds are restated here so
		// that bad CLI input comes back as a diagnostic instead of a stack
		// trace. The duplication is deliberate and the two sides can drift —
		// the constructor's doc comment carries the matching cross-reference.
		period := math.Round(1e9 / cfg.rate)
		if period < 1 {
			return config{}, fmt.Errorf("-rate must be <= 2e9 for -algo=%s (or =all): atomic stores an integer emission interval, and %g/s needs %g ns, below its 1 ns resolution", algoAtomic, cfg.rate, 1e9/cfg.rate)
		}
		// period is confirmed >= 1 above, so converting it to int64 here is safe
		// (an out-of-range float-to-int64 conversion is unspecified in Go, which
		// is exactly why period is validated as a float64 first, as the
		// constructor itself does). The overflow check below is then done in the
		// same int64 arithmetic as NewAtomicTokenBucket's own guard, not in
		// float64: cfg.capacity is bounded well under maxAtomicBucketSpanNanos
		// (see -capacity validation above), so nanosPerToken fits int64 and the
		// division cannot overflow or lose precision the way the float64
		// product period*float64(cfg.capacity) can for spans in
		// (2^62, 2^62+512], which round down to exactly maxAtomicBucketSpanNanos
		// and would otherwise slip past this check and panic in the constructor.
		nanosPerToken := int64(period)
		if int64(cfg.capacity) > maxAtomicBucketSpanNanos/nanosPerToken {
			return config{}, fmt.Errorf("-capacity %d at -rate %g spans %d x %d ns for -algo=%s (or =all), past its %d ns limit: lower -capacity or raise -rate", cfg.capacity, cfg.rate, cfg.capacity, nanosPerToken, algoAtomic, int64(maxAtomicBucketSpanNanos))
		}
	}

	return cfg, nil
}

// containsAlgo reports whether name appears in algos.
func containsAlgo(algos []string, name string) bool {
	for _, a := range algos {
		if a == name {
			return true
		}
	}
	return false
}

// makeLimiter constructs the limiter named by algo from the CLI parameters.
//
// The constructors are deliberately not uniform — NewSlidingWindow takes
// (limit, window) because a sliding window has no notion of a bucket — so this
// is where the CLI's (rate, capacity) pair is mapped onto each algorithm's own
// parameters. The signatures are not to be harmonized for the harness's
// convenience; they reflect the algorithms.
func makeLimiter(algo string, cfg config) (limiter.Limiter, error) {
	switch algo {
	case algoToken:
		return limiter.NewTokenBucket(cfg.rate, cfg.capacity, limiter.SystemClock), nil
	case algoSliding:
		return limiter.NewSlidingWindow(int(cfg.rate), slidingWindowLength, limiter.SystemClock), nil
	case algoLeaky:
		return limiter.NewLeakyBucket(cfg.rate, cfg.capacity, limiter.SystemClock), nil
	case algoLockFree:
		return limiter.NewLockFreeTokenBucket(cfg.rate, cfg.capacity, limiter.SystemClock), nil
	case algoAtomic:
		// Bounds pre-checked in parseConfig; see the note there.
		return limiter.NewAtomicTokenBucket(cfg.rate, cfg.capacity, limiter.SystemClock), nil
	default:
		return nil, fmt.Errorf("unknown algorithm %q", algo)
	}
}

// run executes the whole harness: build each limiter, drive it, render the
// table. It is separate from main so that the exit path stays in one place.
func run(args []string, stdout io.Writer) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}

	results := make([]result, 0, len(cfg.algos))
	for _, algo := range cfg.algos {
		lim, err := makeLimiter(algo, cfg)
		if err != nil {
			return err
		}
		results = append(results, runOne(algo, lim, cfg))
	}

	if cfg.format == formatMarkdown {
		return renderMarkdown(stdout, results, cfg)
	}
	return renderText(stdout, results, cfg)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Usage has already been printed by parseConfig.
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}
