package radixhash

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

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
