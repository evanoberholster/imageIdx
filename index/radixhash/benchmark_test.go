package radixhash

import (
	"bytes"
	"fmt"
	"math/bits"
	"math/rand"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/index/radixhash/internal/tailkernel"
	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// This file holds every benchmark in the package plus their shared helpers.
// The fixed-workload benchmarks (apply, promotion small writes, compact) ignore
// b.N and run a fixed amount of work per invocation; run them with
// -benchtime=1x. The b.N-driven benchmarks (lookup, layout, leaf search) run
// normally.

// layoutHash derives a deterministic distinct 32-byte digest from an index.
func layoutHash(i uint64) Hash {
	var h Hash
	x := i*0x9e3779b97f4a7c15 + 1
	for w := 0; w < 4; w++ {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z = z ^ (z >> 31)
		for b := 0; b < 8; b++ {
			h[w*8+b] = byte(z >> (8 * uint(b)))
		}
	}
	return h
}

// skewHash concentrates hashes into 16 root/level-1 prefixes so buckets are
// large and promotion recurses, contrasting uniform cryptographic hashes.
func skewHash(i uint64) Hash {
	h := layoutHash(i)
	h[0] = byte(i % 4)
	h[1] = byte((i / 4) % 4)
	return h
}

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
	switch {
	case batchSize == 1:
		iters = 20000
	case batchSize >= 4096:
		iters = 200
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
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/float64(iters), "alloc_B/apply")
	b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(iters), "mallocs/apply")
	b.ReportMetric(float64(index.Metadata().Depth), "depth")
}

// BenchmarkApplySmallBatch is the primary Apply benchmark: batches of 1-64
// records (plus a 4096 maintenance batch), the sizes ImageIdx normally issues.
func BenchmarkApplySmallBatch(b *testing.B) {
	for _, batchSize := range []int{1, 8, 16, 32, 64, 4096} {
		for _, pattern := range []string{"uniform", "same-root", "same-prefix", "mixed"} {
			b.Run(fmt.Sprintf("%s/batch=%d", pattern, batchSize), func(b *testing.B) {
				benchApplySmall(b, batchSize, pattern)
			})
		}
	}
}

// BenchmarkCompact measures streaming compaction throughput, allocations, and
// heap growth at increasing sizes.
func BenchmarkCompact(b *testing.B) {
	for _, n := range []int{100_000, 1_000_000, 10_000_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			index, err := New(uint64(n)*64+(256<<20), Config{AutoReclaimBytes: 64 << 20})
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			const batchSize = 1 << 16
			batch := make([]Entry, 0, batchSize)
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

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			if err := index.Compact(); err != nil {
				b.Fatalf("Compact: %v", err)
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)

			allocated := after.TotalAlloc - before.TotalAlloc
			heapGrowth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			b.ReportMetric(elapsed.Seconds(), "compact_s")
			b.ReportMetric(float64(n)/elapsed.Seconds(), "entries/s")
			b.ReportMetric(float64(allocated)/float64(n), "compact_alloc_B/entry")
			b.ReportMetric(float64(heapGrowth)/(1<<20), "heap_growth_MiB")
			b.ReportMetric(float64(compactBatchEntries), "batch_entries")
			b.Logf("n=%d compact=%s alloc=%d heap_growth=%dMiB batch=%d",
				n, elapsed, allocated, heapGrowth>>20, compactBatchEntries)

			if err := index.Close(); err != nil {
				b.Fatalf("Close: %v", err)
			}
		})
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

// benchLayout builds n entries in one of the two root shapes and reports build
// throughput, index bytes/entry, cumulative COW allocation bytes/entry, lookup
// latency, and lookup allocations. forceRadix selects Root->Radix->Leaf;
// otherwise a small bucket stays a direct Root->Leaf.
func benchLayout(b *testing.B, n int, forceRadix bool) {
	config := Config{
		PromotionBytes:   1 << 30, // never promote past the starting shape
		AutoReclaimBytes: 64 << 20,
		forceRootRadix:   forceRadix,
	}
	arena := uint64(n)*128 + 512<<20
	index, err := New(arena, config)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer index.Close()

	const batchSize = 1 << 16
	batch := make([]Entry, 0, batchSize)
	start := time.Now()
	for i := 0; i < n; {
		batch = batch[:0]
		for k := 0; k < batchSize && i < n; k++ {
			batch = append(batch, Entry{Hash: layoutHash(uint64(i)), ImageID: uint64(i + 1)})
			i++
		}
		if err := index.Apply(batch); err != nil {
			b.Fatalf("Apply at %d: %v", i, err)
		}
	}
	build := time.Since(start)
	meta := index.Metadata()
	used := meta.ArenaBytes - meta.FreeBytes
	cowAlloc := index.alloc.AllocatedBytes()

	const distinct = 8192
	queries := make([]Hash, distinct)
	for i := range queries {
		queries[i] = layoutHash(uint64(i) * uint64(n) / distinct)
	}
	buf := make([]ImageID, 0, 8)
	const samples = 200_000
	lookupStart := time.Now()
	for s := 0; s < samples; s++ {
		buf = index.Lookup(buf[:0], queries[s%distinct])
	}
	lookup := time.Since(lookupStart)
	allocs := testing.AllocsPerRun(1000, func() {
		buf = index.Lookup(buf[:0], queries[0])
	})

	b.ReportMetric(build.Seconds(), "build_s")
	b.ReportMetric(float64(n)/build.Seconds(), "insert/s")
	b.ReportMetric(float64(used)/float64(n), "index_B/entry")
	b.ReportMetric(float64(cowAlloc)/float64(n), "cow_alloc_B/entry")
	b.ReportMetric(float64(meta.Depth), "depth")
	b.ReportMetric(float64(lookup.Nanoseconds())/float64(samples), "lookup_ns/op")
	b.ReportMetric(allocs, "lookup_allocs/op")
	b.Logf("n=%d root-radix=%v build=%s used=%d cow_alloc=%d depth=%d lookup=%.1fns allocs=%.2f",
		n, forceRadix, build, used, cowAlloc, meta.Depth,
		float64(lookup.Nanoseconds())/float64(samples), allocs)
	_ = buf
}

// BenchmarkLayout compares the sparse Root->Leaf shape with the forced
// Root->Radix->Leaf shape at increasing sizes.
func BenchmarkLayout(b *testing.B) {
	for _, n := range []int{1_000, 100_000, 1_000_000, 10_000_000} {
		b.Run(fmt.Sprintf("root-leaf/%d", n), func(b *testing.B) { benchLayout(b, n, false) })
		b.Run(fmt.Sprintf("root-radix/%d", n), func(b *testing.B) { benchLayout(b, n, true) })
	}
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

// benchmarkLookupShape builds a concentrated tree and measures point lookup.
func benchmarkLookupShape(b *testing.B, size int, config Config) {
	// Concentrate hashes into a small number of Hash[0]/Hash[1] buckets so the
	// promotion threshold is exercised.
	index, err := New(64<<20, config)
	if err != nil {
		b.Fatal(err)
	}
	defer index.Close()
	rng := rand.New(rand.NewSource(42))
	hashes := make([]Hash, size)
	batch := make([]Entry, 0, 4096)
	for i := range hashes {
		hashes[i] = hashWithPrefix([]byte{byte(i % 8), byte((i / 8) % 8)}, uint64(rng.Int63())+1)
		batch = append(batch, Entry{Hash: hashes[i], ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(batch); err != nil {
		b.Fatal(err)
	}
	b.Logf("depth=%d", index.Metadata().Depth)
	var dst []ImageID
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.Lookup(dst[:0], hashes[i%len(hashes)])
	}
	_ = dst
}

func BenchmarkLookupTwoLevelVersusThreeLevel(b *testing.B) {
	for _, size := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("two-level/%d", size), func(b *testing.B) {
			benchmarkLookupShape(b, size, Config{PromotionBytes: 1 << 30})
		})
		b.Run(fmt.Sprintf("three-level/%d", size), func(b *testing.B) {
			benchmarkLookupShape(b, size, Config{PromotionBytes: 8 << 10})
		})
	}
}

func BenchmarkApplyBatch(b *testing.B) {
	index, err := New(256<<20, Config{})
	if err != nil {
		b.Fatal(err)
	}
	defer index.Close()
	rng := rand.New(rand.NewSource(3))
	batch := make([]Entry, 256)
	for i := range batch {
		batch[i] = Entry{Hash: hashWithPrefix([]byte{byte(rng.Intn(256)), byte(rng.Intn(256))}, uint64(i)+1), ImageID: ImageID(i + 1)}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := index.Apply(batch); err != nil {
			b.Fatal(err)
		}
		if err := index.Reclaim(); err != nil {
			b.Fatal(err)
		}
	}
}

// scanLeafLinear is the SIMD linear-scan reference used only by
// BenchmarkLeafSearchCrossover. It is not on the production hot path: binary
// search wins at every occupancy and this path allocates because the query
// prefix is passed through an indirect kernel call.
func scanLeafLinear(arena []byte, ref pagealloc.Ref, query Hash, depth int, header leafHeader, visit func(ImageID) bool) error {
	tailLen := tailLenFor(depth)
	want := query[depth:]
	base := extentOffset(ref) + uint64(leafHeaderSize)
	mask := tailkernel.MatchMask(arena[base:], tailLen, header.count, want)
	if mask == 0 {
		return nil
	}
	idsBase := extentOffset(ref) + uint64(header.idsOffset)
	if header.idBytes == id32Bytes {
		ids, err := storage.View[uint32](arena, idsBase, uint64(header.count), ErrCorrupt, ErrCorrupt)
		if err != nil {
			return err
		}
		for mask != 0 {
			i := bits.TrailingZeros64(mask)
			mask &= mask - 1
			off := base + uint64(tailLen*i)
			if bytes.Equal(arena[off:off+uint64(tailLen)], want) && !visit(ImageID(ids[i])) {
				return nil
			}
		}
		return nil
	}
	ids, err := storage.View[uint64](arena, idsBase, uint64(header.count), ErrCorrupt, ErrCorrupt)
	if err != nil {
		return err
	}
	for mask != 0 {
		i := bits.TrailingZeros64(mask)
		mask &= mask - 1
		off := base + uint64(tailLen*i)
		if bytes.Equal(arena[off:off+uint64(tailLen)], want) && !visit(ImageID(ids[i])) {
			return nil
		}
	}
	return nil
}

// layoutLeaf builds one depth-1 leaf holding count distinct slots at pagealloc
// reference 1 and returns a query hash that matches the middle slot, so each
// search confirms a hit.
func layoutLeaf(count int) (arena []byte, ref pagealloc.Ref, header leafHeader, query Hash) {
	slots := make([]slot, count)
	mid := count / 2
	for i := 0; i < count; i++ {
		h := layoutHash(uint64(i) + 1)
		copy(slots[i].tail[:], h[1:])
		slots[i].id = uint64(i + 1)
	}
	sortSlots(slots, tailLenFor(1))
	extentBytes := uint32(((sizeForCapacity(1, id32Bytes, count) + 511) / 512) * 512)
	arena = make([]byte, int(pagealloc.MinBlockSize)+int(extentBytes))
	if err := encodeLeaf(arena[int(pagealloc.MinBlockSize):], 1, slots); err != nil {
		panic(err)
	}
	ref = pagealloc.Ref(1)
	header, err := parseLeafHeader(arena, ref)
	if err != nil {
		panic(err)
	}
	return arena, ref, header, layoutHash(uint64(mid) + 1)
}

func benchLeafSearch(b *testing.B, count int) {
	arena, ref, header, query := layoutLeaf(count)
	const depth = 1
	visit := func(ImageID) bool { return true }
	b.Run("linear", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = scanLeafLinear(arena, ref, query, depth, header, visit)
		}
	})
	b.Run("binary", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = scanLeafBinary(arena, ref, query, depth, header, visit)
		}
	})
	b.Run("dispatch", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = scanLeaf(arena, ref, query, depth, visit)
		}
	})
}

// BenchmarkLeafSearchCrossover measures the SIMD linear scan against binary
// search across realistic small-leaf occupancies. Linear is only valid up to
// tailkernel's 64-slot mask, so the crossover lives in this range.
func BenchmarkLeafSearchCrossover(b *testing.B) {
	for _, count := range []int{4, 8, 12, 16, 24, 32, 48, 64} {
		b.Run(fmt.Sprintf("count=%d", count), func(b *testing.B) { benchLeafSearch(b, count) })
	}
}

// partitionSlotsMap is the previous map-based 256-way partition, kept only for
// the BenchmarkPartition comparison.
func partitionSlotsMap(slots []slot, tailLen int) map[byte][]slot {
	groups := make(map[byte][]slot)
	for i := range slots {
		current := &slots[i]
		key := current.tail[0]
		var child slot
		child.id = current.id
		copy(child.tail[:], current.tail[1:tailLen])
		groups[key] = append(groups[key], child)
	}
	return groups
}

// BenchmarkPartition compares the fixed-array 256-way partition against the
// previous map-based implementation for realistic group sizes.
func BenchmarkPartition(b *testing.B) {
	const tailLen = 31
	for _, n := range []int{1, 8, 16, 32, 64, 256, 4096} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			slots := make([]slot, n)
			for i := range slots {
				h := layoutHash(uint64(i) + 1)
				copy(slots[i].tail[:], h[1:])
				slots[i].id = uint64(i + 1)
			}
			dst := make([]slot, n)
			b.Run("fixed", func(b *testing.B) {
				var counts [radixFanout]uint32
				var offsets [radixFanout + 1]uint32
				var sum uint64
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					partitionSlots(dst, slots, tailLen, &counts, &offsets)
					sum += uint64(offsets[radixFanout])
				}
				if sum == 0 {
					b.Fatal("empty partition")
				}
			})
			b.Run("map", func(b *testing.B) {
				var out map[byte][]slot
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					out = partitionSlotsMap(slots, tailLen)
				}
				if len(out) == 0 {
					b.Fatal("empty partition")
				}
			})
		})
	}
}
