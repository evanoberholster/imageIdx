package radixhash

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// skewHash concentrates hashes into 16 root/level-1 prefixes so buckets are
// large and promotion recurses, contrasting uniform cryptographic hashes.
func skewHash(i uint64) Hash {
	h := layoutHash(i)
	h[0] = byte(i % 4)
	h[1] = byte((i / 4) % 4)
	return h
}

// treeStats summarizes a published view without modifying it.
type treeStats struct {
	depth     int
	leaves    int
	entries   uint64
	occupancy []int
	sizes     []int
}

func collectTreeStats(view *indexView) treeStats {
	var stats treeStats
	record := func(h leafHeader) {
		stats.leaves++
		stats.entries += uint64(h.count)
		stats.occupancy = append(stats.occupancy, h.count)
		stats.sizes = append(stats.sizes, int(h.extentBytes))
		if h.depth > stats.depth {
			stats.depth = h.depth
		}
	}
	var walk func(ref pagealloc.Ref, level int)
	walk = func(ref pagealloc.Ref, level int) {
		if level >= maxDepth || !validExtent(view.arena, ref, radixNodeBytes) {
			return
		}
		for i := 0; i < radixFanout; i++ {
			child := nodeChild(view.arena, ref, byte(i))
			if child == 0 {
				continue
			}
			childRef, isNode := decodeChild(child)
			if isNode {
				walk(childRef, level+1)
				continue
			}
			if h, err := readLeafHeader(view.arena, childRef); err == nil {
				record(h)
			}
		}
	}
	for i := 0; i < radixFanout; i++ {
		child := view.root[i]
		if child == 0 {
			continue
		}
		childRef, isNode := decodeChild(child)
		if isNode {
			walk(childRef, 1)
			continue
		}
		if h, err := readLeafHeader(view.arena, childRef); err == nil {
			record(h)
		}
	}
	return stats
}

func percentile(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	index := int(p*float64(len(sorted)-1) + 0.5)
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func thresholdLabel(bytes uint32) string {
	return fmt.Sprintf("%dKiB", bytes>>10)
}

// benchPromotion builds n entries at one promotion threshold and reports the
// full tradeoff: insertion, lookup, space, allocation, tree shape, and
// compaction throughput.
func benchPromotion(b *testing.B, n int, threshold uint32, skew bool) {
	config := Config{PromotionBytes: threshold, AutoReclaimBytes: 64 << 20}
	index, err := New(uint64(n)*64+(512<<20), config)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer index.Close()

	const batchSize = 1 << 16
	batch := make([]Entry, 0, batchSize)
	buildStart := time.Now()
	for i := 0; i < n; {
		batch = batch[:0]
		for k := 0; k < batchSize && i < n; k++ {
			h := layoutHash(uint64(i))
			if skew {
				h = skewHash(uint64(i))
			}
			batch = append(batch, Entry{Hash: h, ImageID: uint64(i + 1)})
			i++
		}
		if err := index.Apply(batch); err != nil {
			b.Fatalf("Apply at %d: %v", i, err)
		}
	}
	build := time.Since(buildStart)

	meta := index.Metadata()
	used := meta.ArenaBytes - meta.FreeBytes
	allocated := index.alloc.AllocatedBytes()
	stats := collectTreeStats(index.view.Load())
	sort.Ints(stats.sizes)
	avgOccupancy := 0.0
	if stats.leaves > 0 {
		total := 0
		for _, c := range stats.occupancy {
			total += c
		}
		avgOccupancy = float64(total) / float64(stats.leaves)
	}

	const distinct = 8192
	queries := make([]Hash, distinct)
	for i := range queries {
		src := uint64(i) * uint64(n) / distinct
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
	allocs := testing.AllocsPerRun(1000, func() { buf = index.Lookup(buf[:0], queries[0]) })

	compactStart := time.Now()
	if err := index.Compact(); err != nil {
		b.Fatalf("Compact: %v", err)
	}
	compact := time.Since(compactStart)

	b.ReportMetric(build.Seconds(), "build_s")
	b.ReportMetric(float64(n)/build.Seconds(), "insert/s")
	b.ReportMetric(float64(lookup.Nanoseconds())/float64(samples), "lookup_ns/op")
	b.ReportMetric(allocs, "lookup_allocs/op")
	b.ReportMetric(float64(allocated)/float64(n), "allocated_B/entry")
	b.ReportMetric(float64(used)/float64(n), "index_B/entry")
	b.ReportMetric(float64(stats.depth), "depth")
	b.ReportMetric(float64(stats.leaves), "leaves")
	b.ReportMetric(avgOccupancy, "avg_occupancy")
	b.ReportMetric(float64(percentile(stats.sizes, 0.50)), "leaf_p50_B")
	b.ReportMetric(float64(percentile(stats.sizes, 0.95)), "leaf_p95_B")
	b.ReportMetric(float64(percentile(stats.sizes, 0.99)), "leaf_p99_B")
	b.ReportMetric(compact.Seconds(), "compact_s")
	b.ReportMetric(float64(n)/compact.Seconds(), "compact_entries/s")
}

// BenchmarkPromotionSweep sweeps leaf promotion thresholds at representative
// sizes for uniform and skewed hashes.
func BenchmarkPromotionSweep(b *testing.B) {
	thresholds := []uint32{8 << 10, 16 << 10, 32 << 10, 64 << 10, 96 << 10, 128 << 10, 256 << 10}
	for _, n := range []int{100_000, 1_000_000} {
		for _, threshold := range thresholds {
			for _, skew := range []bool{false, true} {
				label := "uniform"
				if skew {
					label = "skewed"
				}
				name := fmt.Sprintf("%s/%d/%s", label, n, thresholdLabel(threshold))
				b.Run(name, func(b *testing.B) { benchPromotion(b, n, threshold, skew) })
			}
		}
	}
}

// BenchmarkPromotionSweep10M covers a few thresholds at 10M entries.
func BenchmarkPromotionSweep10M(b *testing.B) {
	for _, threshold := range []uint32{32 << 10, 96 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("uniform/10000000/%s", thresholdLabel(threshold)), func(b *testing.B) {
			benchPromotion(b, 10_000_000, threshold, false)
		})
	}
}

// BenchmarkBatchSize sweeps realistic insertion batch sizes at 100K entries.
func BenchmarkBatchSize(b *testing.B) {
	const n = 100_000
	for _, batchSize := range []int{1, 64, 1 << 10, 4 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("batch=%d", batchSize), func(b *testing.B) {
			index, err := New(uint64(n)*64+(512<<20), Config{AutoReclaimBytes: 64 << 20})
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			defer index.Close()
			batch := make([]Entry, 0, batchSize)
			start := time.Now()
			for i := 0; i < n; {
				batch = batch[:0]
				for k := 0; k < batchSize && i < n; k++ {
					batch = append(batch, Entry{Hash: layoutHash(uint64(i)), ImageID: uint64(i + 1)})
					i++
				}
				if err := index.Apply(batch); err != nil {
					b.Fatalf("Apply: %v", err)
				}
			}
			elapsed := time.Since(start)
			allocated := index.alloc.AllocatedBytes()
			used := index.Metadata().ArenaBytes - index.Metadata().FreeBytes
			b.ReportMetric(elapsed.Seconds(), "build_s")
			b.ReportMetric(float64(n)/elapsed.Seconds(), "insert/s")
			b.ReportMetric(float64(allocated)/float64(n), "allocated_B/entry")
			b.ReportMetric(float64(used)/float64(n), "index_B/entry")
		})
	}
}
