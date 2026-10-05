package radixhash

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// entryBytes is the in-memory size of one Entry (256-bit hash plus uint64 id).
const entryBytes = hashBytes + 8

// applyPatternHash maps a record index to one of the write patterns ImageIdx
// actually issues: spread across root buckets, all in one root bucket, all
// sharing a deeper prefix, and a mix.
func applyPatternHash(pattern string, i uint64) Hash {
	h := layoutHash(i)
	switch pattern {
	case "same-root":
		h[0] = 0
	case "same-prefix":
		h[0], h[1] = 0, 0
	case "mixed":
		switch i % 4 {
		case 1:
			h[0] = 0
		case 2:
			h[0], h[1] = 0, 0
		case 3:
			h[0] = byte(i % 8)
		}
	}
	return h
}

func buildPatternBase(b *testing.B, index *Index, n int, pattern string) {
	b.Helper()
	const chunk = 1 << 14
	batch := make([]Entry, 0, chunk)
	for i := 0; i < n; {
		batch = batch[:0]
		for k := 0; k < chunk && i < n; k++ {
			batch = append(batch, Entry{Hash: applyPatternHash(pattern, uint64(i)), ImageID: uint64(i + 1)})
			i++
		}
		if err := index.Apply(batch); err != nil {
			b.Fatalf("base Apply at %d: %v", i, err)
		}
	}
}

// benchApplySmall measures the normal ImageIdx write path: a pre-built index,
// then a fixed number of small new-record batches. It reports per-batch and
// per-entry latency, the copy-on-write bytes each entry costs, and allocations.
func benchApplySmall(b *testing.B, batchSize int, pattern string) {
	const base = 100_000
	index, err := New(uint64(base)*64+(256<<20), Config{AutoReclaimBytes: 32 << 20})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer index.Close()
	buildPatternBase(b, index, base, pattern)

	iters := 2000
	if batchSize == 1 {
		iters = 20000
	}
	batch := make([]Entry, batchSize)
	cowBefore := index.alloc.AllocatedBytes()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	next := uint64(base)
	for it := 0; it < iters; it++ {
		for k := range batch {
			batch[k] = Entry{Hash: applyPatternHash(pattern, next), ImageID: next + 1}
			next++
		}
		if err := index.Apply(batch); err != nil {
			b.Fatalf("Apply: %v", err)
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	cowBytes := index.alloc.AllocatedBytes() - cowBefore

	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(iters), "apply_ns/op")
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(iters*batchSize), "apply_ns/entry")
	b.ReportMetric(float64(cowBytes)/float64(iters*batchSize), "cow_B/entry")
	b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(iters), "mallocs/apply")
	b.ReportMetric(float64(index.Metadata().Depth), "depth")
}

// BenchmarkApplySmallBatch is the primary Apply benchmark: batches of 1-64
// records, the sizes ImageIdx normally issues.
func BenchmarkApplySmallBatch(b *testing.B) {
	for _, batchSize := range []int{1, 8, 16, 32, 64} {
		for _, pattern := range []string{"uniform", "same-root", "same-prefix", "mixed"} {
			b.Run(fmt.Sprintf("%s/batch=%d", pattern, batchSize), func(b *testing.B) {
				benchApplySmall(b, batchSize, pattern)
			})
		}
	}
}

// benchSmallWrites builds a base index at one promotion threshold, applies many
// small updates, then measures lookup latency and tree shape so promotion can
// be judged on the small-write workload rather than bulk construction.
func benchSmallWrites(b *testing.B, threshold uint32, skew bool, batchSize int) {
	const base = 200_000
	index, err := New(uint64(base)*64+(256<<20), Config{PromotionBytes: threshold, AutoReclaimBytes: 32 << 20})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer index.Close()

	const chunk = 1 << 14
	build := make([]Entry, 0, chunk)
	for i := 0; i < base; {
		build = build[:0]
		for k := 0; k < chunk && i < base; k++ {
			h := layoutHash(uint64(i))
			if skew {
				h = skewHash(uint64(i))
			}
			build = append(build, Entry{Hash: h, ImageID: uint64(i + 1)})
			i++
		}
		if err := index.Apply(build); err != nil {
			b.Fatalf("base Apply: %v", err)
		}
	}

	iters := 2000
	if batchSize == 1 {
		iters = 10000
	}
	batch := make([]Entry, batchSize)
	cowBefore := index.alloc.AllocatedBytes()
	start := time.Now()
	next := uint64(base)
	for it := 0; it < iters; it++ {
		for k := range batch {
			h := layoutHash(next)
			if skew {
				h = skewHash(next)
			}
			batch[k] = Entry{Hash: h, ImageID: next + 1}
			next++
		}
		if err := index.Apply(batch); err != nil {
			b.Fatalf("Apply: %v", err)
		}
	}
	applyElapsed := time.Since(start)
	cowBytes := index.alloc.AllocatedBytes() - cowBefore

	const distinct = 8192
	queries := make([]Hash, distinct)
	for i := range queries {
		src := uint64(i) * base / distinct
		queries[i] = layoutHash(src)
		if skew {
			queries[i] = skewHash(src)
		}
	}
	buf := make([]ImageID, 0, 8)
	const samples = 100_000
	lookupStart := time.Now()
	for s := 0; s < samples; s++ {
		buf = index.Lookup(buf[:0], queries[s%distinct])
	}
	lookup := time.Since(lookupStart)

	stats := collectTreeStats(index.view.Load())
	meta := index.Metadata()
	used := meta.ArenaBytes - meta.FreeBytes
	avgOccupancy := 0.0
	if stats.leaves > 0 {
		total := 0
		for _, c := range stats.occupancy {
			total += c
		}
		avgOccupancy = float64(total) / float64(stats.leaves)
	}

	b.ReportMetric(float64(applyElapsed.Nanoseconds())/float64(iters), "apply_ns/op")
	b.ReportMetric(float64(applyElapsed.Nanoseconds())/float64(iters*batchSize), "apply_ns/entry")
	b.ReportMetric(float64(cowBytes)/float64(iters*batchSize), "cow_B/entry")
	b.ReportMetric(float64(lookup.Nanoseconds())/float64(samples), "lookup_ns/op")
	b.ReportMetric(float64(used)/float64(meta.Entries), "index_B/entry")
	b.ReportMetric(float64(stats.depth), "depth")
	b.ReportMetric(float64(stats.leaves), "leaves")
	b.ReportMetric(avgOccupancy, "avg_occupancy")
}

// BenchmarkPromotionSmallWrites judges promotion thresholds on repeated small
// updates, for uniform and skewed hashes.
func BenchmarkPromotionSmallWrites(b *testing.B) {
	for _, threshold := range []uint32{32 << 10, 64 << 10, 96 << 10, 128 << 10, 256 << 10} {
		for _, skew := range []bool{false, true} {
			for _, batchSize := range []int{1, 8, 32, 64} {
				label := "uniform"
				if skew {
					label = "skewed"
				}
				b.Run(fmt.Sprintf("%s/%s/batch=%d", label, thresholdLabel(threshold), batchSize), func(b *testing.B) {
					benchSmallWrites(b, threshold, skew, batchSize)
				})
			}
		}
	}
}

// BenchmarkCompactBatch compares internal streaming batches for compaction,
// which is a maintenance workload distinct from normal ingestion.
func BenchmarkCompactBatch(b *testing.B) {
	const n = 1_000_000
	for _, batch := range []int{4 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			original := compactBatchEntries
			compactBatchEntries = batch
			defer func() { compactBatchEntries = original }()

			index, err := New(uint64(n)*64+(256<<20), Config{AutoReclaimBytes: 64 << 20})
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			defer index.Close()
			const chunk = 1 << 16
			entries := make([]Entry, 0, chunk)
			for i := 0; i < n; {
				entries = entries[:0]
				for k := 0; k < chunk && i < n; k++ {
					entries = append(entries, Entry{Hash: layoutHash(uint64(i)), ImageID: uint64(i + 1)})
					i++
				}
				if err := index.Apply(entries); err != nil {
					b.Fatalf("Apply: %v", err)
				}
			}

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			if err := index.Compact(); err != nil {
				b.Fatalf("Compact: %v", err)
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			b.ReportMetric(elapsed.Seconds(), "compact_s")
			b.ReportMetric(float64(n)/elapsed.Seconds(), "entries/s")
			b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/float64(n), "alloc_B/entry")
			b.ReportMetric(float64(batch*entryBytes)/(1<<20), "batch_MiB")
			b.ReportMetric(float64(compactBatchEntries), "batch_entries")
		})
	}
}
