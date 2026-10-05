package radixhash

import (
	"fmt"
	"testing"
	"time"
)

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

// benchLayout builds n entries in one of the two root shapes and reports build
// throughput, index bytes/entry, cumulative bytes written/entry, lookup
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
	written := index.alloc.AllocatedBytes()

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
	b.ReportMetric(float64(written)/float64(n), "write_B/entry")
	b.ReportMetric(float64(meta.Depth), "depth")
	b.ReportMetric(float64(lookup.Nanoseconds())/float64(samples), "lookup_ns/op")
	b.ReportMetric(allocs, "lookup_allocs/op")
	b.Logf("n=%d root-radix=%v build=%s used=%d written=%d depth=%d lookup=%.1fns allocs=%.2f",
		n, forceRadix, build, used, written, meta.Depth,
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
