// Command bench is the standalone benchmark runner for ratelimit-lab.
//
// It drives every limiter implementation under configurable concurrent load and
// prints a comparative throughput/latency report. The Go-native micro-benchmarks
// live in internal/limiter/*_bench_test.go (run with `go test -bench`); this
// binary is the human-facing harness that produces the comparison table shipped
// in the README.
//
// TODO(Этап 5): implement flag parsing (-algo, -goroutines, -rate, -duration),
// the concurrent load driver, and the comparative report renderer.
package main

import (
	"fmt"
	"os"
)

func main() {
	// Placeholder entry point for Этап 0. Real implementation lands in Этап 5.
	fmt.Fprintln(os.Stderr, "ratelimit-lab bench: not implemented yet (Этап 5)")
	os.Exit(0)
}
