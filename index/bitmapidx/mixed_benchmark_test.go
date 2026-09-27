package bitmapidx

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/bench"
)

// Open-loop mixed read/write workload over one persistent store (Bench.MD
// 7, 8, 13, 14). Reader lanes and writer lanes run independent OpenLoop
// schedules at rates derived from bench.MixLabel, so one role's stall never
// pauses the other's scheduler. Writers walk the deterministic bench.Plan
// sequence of inserts and deletes; readers query a deterministic hot-set
// workload. The maintainer checkpoints while both run, and the two recorder
// sets expose whether maintenance hits read or write tail latency.
//
// The workload duration defaults to one second and is overridable with
// IMAGEIDX_BENCH_MIXED_MS so slow machines and CI can shorten it without
// touching -benchtime. The benchmark only runs under `go test -bench`.
const (
	mixedN                 = 100_000
	mixedValues            = 64
	mixedSeed        int64 = 0x5eedc0de
	mixedPlanLen           = 10_000
	mixedReaderLanes       = 4
	mixedWriterLanes       = 2
	mixedOpsPerSec         = 20_000
	mixedReadSamples       = 4_096

	mixedDurationEnv = "IMAGEIDX_BENCH_MIXED_MS"
	mixedDefaultMS   = 1_000
)

func BenchmarkNamespaceIndexMixed(b *testing.B) {
	if testing.Short() {
		b.Skip("mixed persistent workload is benchmark-only; skipped under -short")
	}
	duration := mixedDuration(b)
	for _, mix := range []struct{ write, delete int }{{5, 0}, {5, 5}} {
		mix := mix
		label := fmt.Sprintf("Ratio=%s/N=%d/Duration=%s",
			bench.MixLabel(mix.write, mix.delete), mixedN, duration)
		b.Run(label, func(b *testing.B) {
			benchmarkNamespaceIndexMixedRun(b, duration, mix.write, mix.delete)
		})
	}
}

func mixedDuration(b *testing.B) time.Duration {
	b.Helper()
	milliseconds := mixedDefaultMS
	if raw := os.Getenv(mixedDurationEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			b.Fatalf("%s must be a positive integer, got %q", mixedDurationEnv, raw)
		}
		milliseconds = parsed
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func benchmarkNamespaceIndexMixedRun(b *testing.B, duration time.Duration, writePct, deletePct int) {
	dir := b.TempDir()
	values := make([]Value, mixedValues)
	for i := range values {
		values[i] = StringValue(fmt.Sprintf("v-%d", i))
	}
	heapBefore := bench.HeapBytes()
	store, index := buildMixedStore(b, dir, mixedN, values)
	b.Cleanup(func() { _ = store.Close() })
	bench.ReportIndexSize(b, "NamespaceIndexMixed", heapBefore, bench.HeapBytes(), bench.DiskBytes(dir), mixedN)

	plan := bench.Plan(mixedSeed, mixedPlanLen, writePct, deletePct)
	reads := bench.HotSetWorkload(mixedSeed+1, mixedValues, mixedReadSamples, 0.2, 0.8, nil)

	var last mixedRunResult
	b.ResetTimer()
	for b.Loop() {
		result, err := runMixedWorkload(store, index, plan, reads, values, duration, writePct, deletePct)
		if err != nil {
			b.Fatal(err)
		}
		last = result
	}
	reportMixedRun(b, last, duration)
}

// buildMixedStore creates the persistent enum store and seeds it with n
// memberships spread over the supplied values, then checkpoints so readers
// exercise the mmap-backed base rather than an in-memory delta.
func buildMixedStore(tb testing.TB, dir string, n int, values []Value) (*Index, *NamespaceIndex) {
	tb.Helper()
	store, err := NewIndex(dir, []NamespaceSpec{enumNamespace(common.NamespaceTag)}, NamespaceConfig{}, DurabilityOS)
	if err != nil {
		tb.Fatalf("NewIndex: %v", err)
	}
	index, ok := store.Lookup(common.NamespaceTag)
	if !ok {
		_ = store.Close()
		tb.Fatal("common.NamespaceTag is not registered")
	}
	entries := make([]Entry, 0, MaxNamespaceBatchUpdates)
	flush := func() {
		if len(entries) == 0 {
			return
		}
		commit(tb, index, func(batch *Batch) error { return batch.Add(entries...) })
		entries = entries[:0]
	}
	assignment := bench.NewRNG(mixedSeed).Perm(n)
	for id := 1; id <= n; id++ {
		value := values[assignment[id-1]%len(values)]
		entries = append(entries, Entry{ImageID: ImageID(id), Value: value})
		if len(entries) == cap(entries) {
			flush()
		}
	}
	flush()
	if err := store.Checkpoint(); err != nil {
		_ = store.Close()
		tb.Fatalf("Checkpoint: %v", err)
	}
	return store, index
}

type mixedRunResult struct {
	readRecorders  []*bench.Recorder
	writeRecorders []*bench.Recorder
	readIssued     []int
	writeIssued    []int
	reads          uint64
	writes         uint64
	deletes        uint64
	elapsed        time.Duration
}

// runMixedWorkload runs one duration of reads and mutations. Reader lanes
// open-loop over a deterministic hot-set value workload; writer lanes
// open-loop over the bench.Plan mutation sequence, so inserts and deletes
// stay in plan order across lanes. deletePct == 0 pairs an eviction with
// every insert so population stays bounded in a write-only mix; otherwise
// deletes drain the inserted-ID pool.
func runMixedWorkload(store *Index, index *NamespaceIndex, plan []bench.Op, reads []int, values []Value, duration time.Duration, writePct, deletePct int) (mixedRunResult, error) {
	var mutationCursor, nextID, readCount, writeCount, deleteCount atomic.Uint64
	nextID.Store(mixedN)
	pool := &mixedIDPool{}

	var (
		errOnce sync.Once
		runErr  error
	)
	fail := func(err error) {
		if err != nil {
			errOnce.Do(func() { runErr = err })
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var maintainer sync.WaitGroup
	maintainer.Add(1)
	go func() {
		defer maintainer.Done()
		runMixedMaintenance(ctx, store, mixedMaintenanceInterval(duration), fail)
	}()

	readPct := 100 - writePct - deletePct
	readRate := float64(mixedOpsPerSec) * float64(readPct) / 100
	writeRate := float64(mixedOpsPerSec) * float64(writePct+deletePct) / 100
	readRatePerLane := readRate / float64(mixedReaderLanes)
	writeRatePerLane := writeRate / float64(mixedWriterLanes)

	result := mixedRunResult{
		readRecorders:  make([]*bench.Recorder, mixedReaderLanes),
		writeRecorders: make([]*bench.Recorder, mixedWriterLanes),
		readIssued:     make([]int, mixedReaderLanes),
		writeIssued:    make([]int, mixedWriterLanes),
	}
	var workers sync.WaitGroup
	start := time.Now()
	for lane := 0; lane < mixedReaderLanes; lane++ {
		lane := lane
		result.readRecorders[lane] = bench.NewRecorder(int(readRatePerLane*duration.Seconds()) + 256)
		workers.Add(1)
		go func() {
			defer workers.Done()
			var buffer []ImageID
			position := 0
			result.readIssued[lane] = bench.OpenLoop(duration, readRatePerLane, 1, result.readRecorders[lane], func() {
				readCount.Add(1)
				value := values[reads[position%len(reads)]]
				position++
				bitmap, err := index.MatchValues(value)
				if err != nil {
					fail(err)
					return
				}
				buffer = bitmap.AppendTo(buffer[:0])
				if !mixedSortedUnique(buffer) {
					fail(fmt.Errorf("read returned unsorted or duplicate IDs (n=%d)", len(buffer)))
				}
			})
		}()
	}
	for lane := 0; lane < mixedWriterLanes; lane++ {
		lane := lane
		result.writeRecorders[lane] = bench.NewRecorder(int(writeRatePerLane*duration.Seconds()) + 256)
		workers.Add(1)
		go func() {
			defer workers.Done()
			batch := index.NewBatch()
			defer func() { _ = batch.Close() }()
			result.writeIssued[lane] = bench.OpenLoop(duration, writeRatePerLane, 1, result.writeRecorders[lane], func() {
				switch nextMutation(plan, &mutationCursor) {
				case bench.Write:
					writeCount.Add(1)
					id := ImageID(nextID.Add(1))
					entry := Entry{ImageID: id, Value: values[id%uint64(len(values))]}
					if err := batch.Reset(); err != nil {
						fail(err)
						return
					}
					if err := batch.Add(entry); err != nil {
						fail(err)
						return
					}
					if deletePct == 0 {
						if evicted, ok := pool.pop(); ok {
							if err := batch.Remove(evicted); err != nil {
								fail(err)
								return
							}
						}
					}
					if err := batch.Apply(); err != nil {
						fail(err)
						return
					}
					if err := index.Barrier(); err != nil {
						fail(err)
						return
					}
					pool.push(entry)
				case bench.Delete:
					deleteCount.Add(1)
					evicted, ok := pool.pop()
					if !ok {
						return
					}
					if err := batch.Reset(); err != nil {
						fail(err)
						return
					}
					if err := batch.Remove(evicted); err != nil {
						fail(err)
						return
					}
					if err := batch.Apply(); err != nil {
						fail(err)
						return
					}
					if err := index.Barrier(); err != nil {
						fail(err)
						return
					}
				}
			})
		}()
	}
	workers.Wait()
	result.elapsed = time.Since(start)
	cancel()
	maintainer.Wait()
	if runErr != nil {
		return mixedRunResult{}, runErr
	}
	result.reads = readCount.Load()
	result.writes = writeCount.Load()
	result.deletes = deleteCount.Load()
	return result, nil
}

// nextMutation returns the next Write or Delete in plan order, skipping Read
// entries. The shared cursor keeps the mutation sequence deterministic even
// though lanes execute it concurrently.
func nextMutation(plan []bench.Op, cursor *atomic.Uint64) bench.Op {
	for {
		step := cursor.Add(1) - 1
		if op := plan[step%uint64(len(plan))]; op != bench.Read {
			return op
		}
	}
}

// mixedMaintenanceInterval keeps at least two maintenance windows inside the
// configured duration and never ticks faster than 10ms.
func mixedMaintenanceInterval(duration time.Duration) time.Duration {
	interval := duration / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	return interval
}

// runMixedMaintenance checkpoints while the lanes run and vacuums every
// fourth window. Checkpoint blocks writers, not readers, so any stall it
// imposes shows up in the recorded write and subsequent request latencies.
func runMixedMaintenance(ctx context.Context, store *Index, interval time.Duration, fail func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticks++
			var err error
			if ticks%4 == 0 {
				err = store.Vacuum()
			} else {
				err = store.Checkpoint()
			}
			if err != nil {
				fail(fmt.Errorf("maintenance: %w", err))
				return
			}
		}
	}
}

// mixedIDPool is the FIFO of writer-inserted memberships that removals draw
// from. Compaction keeps the backing slice bounded when the head advances.
type mixedIDPool struct {
	mu   sync.Mutex
	ids  []Entry
	head int
}

func (p *mixedIDPool) push(entry Entry) {
	p.mu.Lock()
	p.ids = append(p.ids, entry)
	p.mu.Unlock()
}

func (p *mixedIDPool) pop() (Entry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.head == len(p.ids) {
		p.ids = p.ids[:0]
		p.head = 0
		return Entry{}, false
	}
	entry := p.ids[p.head]
	p.head++
	if p.head >= 4_096 && p.head*2 >= len(p.ids) {
		p.ids = p.ids[:copy(p.ids, p.ids[p.head:])]
		p.head = 0
	}
	return entry, true
}

func mixedSortedUnique(ids []ImageID) bool {
	if !slices.IsSorted(ids) {
		return false
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return false
		}
	}
	return true
}

// reportMixedRun logs one Recorder distribution per reader and writer lane
// and reports achieved throughput. Reader and writer p99/max are aggregated
// separately as the worst lane, never averaged.
func reportMixedRun(b *testing.B, result mixedRunResult, configured time.Duration) {
	b.Helper()
	total := 0
	for _, count := range result.readIssued {
		total += count
	}
	for _, count := range result.writeIssued {
		total += count
	}
	seconds := result.elapsed.Seconds()
	if seconds <= 0 {
		seconds = 1
	}
	b.ReportMetric(float64(total)/seconds, "ops/s")
	b.ReportMetric(float64(result.reads)/seconds, "reads/s")
	b.ReportMetric(float64(result.writes)/seconds, "writes/s")
	b.ReportMetric(float64(result.deletes)/seconds, "deletes/s")

	var readP99, readMax, writeP99, writeMax time.Duration
	for lane, recorder := range result.readRecorders {
		if recorder == nil {
			continue
		}
		recorder.Report(b, fmt.Sprintf("Read/Lane=%d", lane))
		readP99 = maxDuration(readP99, recorder.Percentile(0.99))
		readMax = maxDuration(readMax, recorder.Max())
	}
	for lane, recorder := range result.writeRecorders {
		if recorder == nil {
			continue
		}
		recorder.Report(b, fmt.Sprintf("Write/Lane=%d", lane))
		writeP99 = maxDuration(writeP99, recorder.Percentile(0.99))
		writeMax = maxDuration(writeMax, recorder.Max())
	}
	b.Logf("[Mixed] duration=%s ops=%d reads=%d writes=%d deletes=%d worst-read p99=%v max=%v worst-write p99=%v max=%v",
		configured, total, result.reads, result.writes, result.deletes,
		readP99, readMax, writeP99, writeMax)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
