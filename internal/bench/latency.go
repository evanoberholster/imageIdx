package bench

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// Recorder aggregates latency samples for workload benchmarks. Record must
// be called from one goroutine at a time; OpenLoop and ClosedLoop keep a
// private recorder per worker and merge them on the caller's goroutine, so
// a shared Recorder never sees concurrent writes.
type Recorder struct {
	samples []time.Duration
	sorted  bool
}

// NewRecorder returns a Recorder preallocated for capacity samples.
func NewRecorder(capacity int) *Recorder {
	if capacity < 0 {
		capacity = 0
	}
	return &Recorder{samples: make([]time.Duration, 0, capacity)}
}

// Record adds one latency sample.
func (r *Recorder) Record(d time.Duration) {
	r.samples = append(r.samples, d)
	r.sorted = false
}

// Count returns the number of recorded samples.
func (r *Recorder) Count() int { return len(r.samples) }

func (r *Recorder) ensureSorted() {
	if !r.sorted {
		sort.Slice(r.samples, func(i, j int) bool { return r.samples[i] < r.samples[j] })
		r.sorted = true
	}
}

// Percentile returns the p-quantile latency for p in [0,1]. An empty
// Recorder returns zero.
func (r *Recorder) Percentile(p float64) time.Duration {
	if len(r.samples) == 0 {
		return 0
	}
	if p <= 0 {
		r.ensureSorted()
		return r.samples[0]
	}
	if p > 1 {
		p = 1
	}
	r.ensureSorted()
	return r.samples[int(p*float64(len(r.samples)-1))]
}

// Max returns the largest recorded latency, or zero when empty.
func (r *Recorder) Max() time.Duration {
	if len(r.samples) == 0 {
		return 0
	}
	r.ensureSorted()
	return r.samples[len(r.samples)-1]
}

// Report logs the standard latency distribution with a stable label.
func (r *Recorder) Report(tb testing.TB, label string) {
	tb.Helper()
	tb.Logf("[%s] n=%d p50=%v p95=%v p99=%v p99.9=%v max=%v",
		label, r.Count(), r.Percentile(0.50), r.Percentile(0.95),
		r.Percentile(0.99), r.Percentile(0.999), r.Max())
}

// Merge appends every sample from other and clears the sorted flag. It is
// how the loop runners fold per-worker recorders back into the caller's.
func (r *Recorder) Merge(other *Recorder) {
	if other == nil || len(other.samples) == 0 {
		return
	}
	r.samples = append(r.samples, other.samples...)
	r.sorted = false
}

// OpenLoop issues requests at ratePerSec arrivals for duration across
// concurrency workers and records each request's latency from its intended
// arrival time. A stalled operation does not pause the scheduler, so the
// stall shows up in later requests' latency instead of being hidden by
// coordinated omission. It returns the number of issued requests.
func OpenLoop(duration time.Duration, ratePerSec float64, concurrency int, rec *Recorder, op func()) int {
	if concurrency < 1 {
		concurrency = 1
	}
	if ratePerSec <= 0 {
		ratePerSec = 1
	}
	interval := time.Duration(float64(time.Second) / ratePerSec)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	jobs := make(chan time.Time, concurrency*2)
	locals := make([]*Recorder, concurrency)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		if rec != nil {
			locals[w] = NewRecorder(0)
		}
		wg.Add(1)
		go func(local *Recorder) {
			defer wg.Done()
			for scheduled := range jobs {
				op()
				if local != nil {
					local.Record(time.Since(scheduled))
				}
			}
		}(locals[w])
	}
	next := time.Now()
	deadline := next.Add(duration)
	issued := 0
	for next.Before(deadline) {
		if wait := time.Until(next); wait > 0 {
			time.Sleep(wait)
		}
		jobs <- next
		next = next.Add(interval)
		issued++
	}
	close(jobs)
	wg.Wait()
	for _, local := range locals {
		rec.Merge(local)
	}
	return issued
}

// ClosedLoop runs n operations partitioned across concurrency workers,
// recording each operation's latency. It is closed-loop: the next operation
// on a worker starts only after the previous one finishes, so a stall
// reduces the number of completed operations beneath a constant rate.
func ClosedLoop(n, concurrency int, rec *Recorder, op func()) int {
	if n < 0 {
		n = 0
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > n && n > 0 {
		concurrency = n
	}
	if n == 0 {
		return 0
	}
	per := n / concurrency
	extra := n % concurrency
	locals := make([]*Recorder, concurrency)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		count := per
		if w < extra {
			count++
		}
		if rec != nil {
			locals[w] = NewRecorder(count)
		}
		wg.Add(1)
		go func(count int, local *Recorder) {
			defer wg.Done()
			for i := 0; i < count; i++ {
				start := time.Now()
				op()
				if local != nil {
					local.Record(time.Since(start))
				}
			}
		}(count, locals[w])
	}
	wg.Wait()
	for _, local := range locals {
		rec.Merge(local)
	}
	return n
}
