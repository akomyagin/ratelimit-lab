package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akomyagin/ratelimit-lab/internal/limiter"
)

// result is one algorithm's raw measurement. Only raw counters live here;
// every derived figure (throughput, avg latency, allocs/op, percentiles) is
// computed by the renderer, so the driver has exactly one job.
type result struct {
	algo    string
	calls   uint64
	allowed uint64
	elapsed time.Duration
	allocs  uint64       // delta of runtime.MemStats.Mallocs over the run
	hist    *latencyHist // nil unless -percentiles was requested
	workers int          // goroutines that produced the counters
}

// workerResult is one goroutine's tally, written exactly once after its loop
// has finished. Keeping the counters in locals until then is what makes the
// measurement honest: a shared atomic counter in the hot loop would add its own
// contention on top of the limiter's, and the limiter's contention is the thing
// being measured. (This is the opposite of the stress-test rule in SKILL §5,
// where contention is the subject of the test rather than the instrument.)
type workerResult struct {
	calls   uint64
	allowed uint64
	hist    *latencyHist
}

// runOne drives lim from cfg.goroutines goroutines for cfg.duration and returns
// the raw counters.
//
// Termination is a single atomic.Bool set by a time.AfterFunc rather than a
// per-call deadline check: an almost-always-read atomic load is close to free
// and creates no contention, whereas calling time.Now() on every iteration just
// to compare against a deadline would measure the clock as much as the limiter.
//
// Allocation accounting is process-wide (runtime.MemStats.Mallocs before and
// after), hence approximate: the runtime itself allocates during the run. It is
// still the number that matters, because the Этап 4 snapshot design allocates
// per successful CAS and that cost has to be visible rather than buried.
func runOne(algo string, lim limiter.Limiter, cfg config) result {
	var stop atomic.Bool
	workers := make([]workerResult, cfg.goroutines)

	var startGate sync.WaitGroup
	startGate.Add(1)
	var wg sync.WaitGroup
	wg.Add(cfg.goroutines)

	for i := 0; i < cfg.goroutines; i++ {
		go func(w *workerResult) {
			defer wg.Done()
			startGate.Wait()

			var calls, allowed uint64
			if cfg.percentiles {
				h := &latencyHist{}
				for !stop.Load() {
					t := time.Now()
					ok := lim.Allow()
					h.observe(time.Since(t))
					calls++
					if ok {
						allowed++
					}
				}
				w.hist = h
			} else {
				for !stop.Load() {
					if lim.Allow() {
						allowed++
					}
					calls++
				}
			}
			w.calls = calls
			w.allowed = allowed
		}(&workers[i])
	}

	// Measure allocations around the loaded section only; the goroutines are
	// parked on startGate until the baseline has been taken.
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	timer := time.AfterFunc(cfg.duration, func() { stop.Store(true) })
	defer timer.Stop()

	start := time.Now()
	startGate.Done()
	wg.Wait()
	elapsed := time.Since(start)

	runtime.ReadMemStats(&m1)

	res := result{
		algo:    algo,
		elapsed: elapsed,
		allocs:  m1.Mallocs - m0.Mallocs,
		workers: cfg.goroutines,
	}
	for i := range workers {
		res.calls += workers[i].calls
		res.allowed += workers[i].allowed
		if workers[i].hist != nil {
			if res.hist == nil {
				res.hist = &latencyHist{}
			}
			res.hist.merge(workers[i].hist)
		}
	}
	return res
}
