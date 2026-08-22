package main

import (
	"fmt"
	"io"
	"math"
	"math/bits"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

// Output formats accepted by -format.
const (
	formatText     = "text"
	formatMarkdown = "markdown"
)

// percentileNote is printed above the table whenever -percentiles is on.
//
// It is not optional politeness: timing every individual Allow() call costs a
// pair of time.Now() readings whose cost is of the same order as Allow() itself,
// and that overhead lands inside every figure in the table, not just the
// percentile columns. Reporting percentiles without saying so would present a
// measurement of "limiter plus stopwatch" as a measurement of the limiter.
const percentileNote = `note: -percentiles adds a time.Now() pair around every Allow() call; this
overhead is comparable to the cost of Allow() itself and is included in ALL
reported figures (throughput, avg, percentiles). The overhead is a roughly
constant cost per call, so it makes up a smaller share of the total for
whichever algorithm already does more work per call — percentile mode
systematically narrows the gaps between rows.`

// deniedWarning is printed above the table whenever any row recorded a
// denial.
//
// A denied call times a different code path than an admitted one — for the
// lock-free algorithm in particular, a deny on an empty bucket returns
// without a CAS and without an allocation (see lockfree_tokenbucket.go), so
// the mutex/CAS comparison this harness exists to make is not valid on rows
// that deny. The warning is deliberately not "denials are a bug": a mismatched
// -rate/-capacity for the workload denies on purpose, and readers need to
// know the row in front of them stopped measuring admission mechanics.
const deniedWarningFormat = `warning: %d row(s) recorded denials; those rows time the rejection path, not
the admission mechanics, and the mutex/CAS comparison is not valid for them.`

// anyDenied reports whether at least one result recorded a denial, and how
// many did — the count that goes into deniedWarningFormat.
func countDenied(rs []result) int {
	n := 0
	for _, r := range rs {
		if derive(r).denied > 0 {
			n++
		}
	}
	return n
}

// latencyHist is a fixed-size log2 histogram of call latencies.
//
// Storing every sample is not an option — a two-second run produces tens of
// millions of them — so samples are bucketed by the bit length of their
// nanosecond value. That is 64 counters, zero allocation in the hot loop, and a
// resolution of one power of two: a reported percentile is correct within a
// factor of two. For ranking four algorithms against each other that is enough,
// and it is more honest than printing a nanosecond figure that the sampling
// method cannot support.
type latencyHist struct {
	buckets [64]uint64
}

// observe records one latency sample. Sub-nanosecond and negative values (a
// clock that failed to advance) land in bucket 0.
func (h *latencyHist) observe(d time.Duration) {
	ns := d.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	idx := bits.Len64(uint64(ns))
	if idx > 63 {
		idx = 63
	}
	h.buckets[idx]++
}

// merge folds another histogram into h element-wise. Used to combine per-worker
// histograms once their goroutines have finished.
func (h *latencyHist) merge(o *latencyHist) {
	if o == nil {
		return
	}
	for i := range h.buckets {
		h.buckets[i] += o.buckets[i]
	}
}

// bucketUpper returns the inclusive upper bound of bucket i: bucket 0 holds
// exactly 0ns, and bucket i>0 holds [2^(i-1), 2^i - 1] ns.
func bucketUpper(i int) time.Duration {
	if i <= 0 {
		return 0
	}
	return time.Duration(uint64(1)<<uint(i) - 1)
}

// quantile returns the upper bound of the bucket covering the q-th quantile
// (q in [0,1]). An empty histogram returns 0 rather than panicking.
func (h *latencyHist) quantile(q float64) time.Duration {
	var total uint64
	for _, c := range h.buckets {
		total += c
	}
	if total == 0 {
		return 0
	}

	target := uint64(math.Ceil(q * float64(total)))
	if target == 0 {
		target = 1
	}
	if target > total {
		target = total
	}

	var cum uint64
	for i, c := range h.buckets {
		cum += c
		if cum >= target {
			return bucketUpper(i)
		}
	}
	return bucketUpper(len(h.buckets) - 1)
}

// derived holds the human-facing figures computed from a raw result. Keeping
// the arithmetic in one place means the text and markdown renderers cannot
// disagree about what a column means.
type derived struct {
	denied     uint64
	throughput float64 // calls per wall-clock second
	avgNs      float64 // wall time * workers / calls: mean cost of one call
	allocsPerO float64
	p50        time.Duration
	p99        time.Duration
	p999       time.Duration
}

func derive(r result) derived {
	d := derived{denied: r.calls - r.allowed}
	if secs := r.elapsed.Seconds(); secs > 0 {
		d.throughput = float64(r.calls) / secs
	}
	if r.calls > 0 {
		d.avgNs = float64(r.elapsed.Nanoseconds()) * float64(r.workers) / float64(r.calls)
		d.allocsPerO = float64(r.allocs) / float64(r.calls)
	}
	if r.hist != nil {
		d.p50 = r.hist.quantile(0.50)
		d.p99 = r.hist.quantile(0.99)
		d.p999 = r.hist.quantile(0.999)
	}
	return d
}

// contextLine describes the run parameters so a pasted table stays
// interpretable away from the command line that produced it.
func contextLine(cfg config) string {
	return fmt.Sprintf("goroutines=%d rate=%g capacity=%d duration=%s %s",
		cfg.goroutines, cfg.rate, cfg.capacity, cfg.duration, runtime.Version())
}

// columnHeaders returns the table header, with the percentile columns appended
// only when they were actually measured.
func columnHeaders(cfg config) []string {
	h := []string{
		"algo", "goroutines", "calls", "allowed", "denied",
		"throughput (calls/s)", "avg latency (ns)", "allocs/op (approx)",
	}
	if cfg.percentiles {
		h = append(h, "p50", "p99", "p99.9")
	}
	return h
}

// rowCells renders one result as strings, in the same order as columnHeaders.
func rowCells(r result, cfg config) []string {
	d := derive(r)
	cells := []string{
		r.algo,
		fmt.Sprintf("%d", r.workers),
		fmt.Sprintf("%d", r.calls),
		fmt.Sprintf("%d", r.allowed),
		fmt.Sprintf("%d", d.denied),
		fmt.Sprintf("%.0f", d.throughput),
		fmt.Sprintf("%.1f", d.avgNs),
		fmt.Sprintf("%.2f", d.allocsPerO),
	}
	if cfg.percentiles {
		cells = append(cells, d.p50.String(), d.p99.String(), d.p999.String())
	}
	return cells
}

// renderText writes the comparison as a column-aligned plain-text table.
func renderText(w io.Writer, rs []result, cfg config) error {
	if cfg.percentiles {
		if _, err := fmt.Fprintln(w, percentileNote); err != nil {
			return err
		}
	}
	if n := countDenied(rs); n > 0 {
		if _, err := fmt.Fprintf(w, deniedWarningFormat+"\n", n); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, contextLine(cfg)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(columnHeaders(cfg), "\t")); err != nil {
		return err
	}
	for _, r := range rs {
		if _, err := fmt.Fprintln(tw, strings.Join(rowCells(r, cfg), "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// renderMarkdown writes the comparison as a GFM table, ready to paste into the
// README.
func renderMarkdown(w io.Writer, rs []result, cfg config) error {
	if cfg.percentiles {
		if _, err := fmt.Fprintln(w, percentileNote); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if n := countDenied(rs); n > 0 {
		if _, err := fmt.Fprintf(w, deniedWarningFormat+"\n\n", n); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "%s\n\n", contextLine(cfg)); err != nil {
		return err
	}

	headers := columnHeaders(cfg)
	if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(headers, " | ")); err != nil {
		return err
	}
	sep := make([]string, len(headers))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := fmt.Fprintf(w, "|%s|\n", strings.Join(sep, "|")); err != nil {
		return err
	}
	for _, r := range rs {
		if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(rowCells(r, cfg), " | ")); err != nil {
			return err
		}
	}
	return nil
}
