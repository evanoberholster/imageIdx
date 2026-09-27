package roar

import (
	"fmt"
	"sync"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/bench"
)

// BenchmarkBitmapConcurrency reports read scaling rather than assuming more
// goroutines help: the same immutable AndCardinality workload is split
// across a fixed worker count, so a flat or falling ops/sec curve exposes
// lock, atomic, or memory-bandwidth saturation. Bitmaps are immutable, so
// scaling is expected to be near linear until memory bandwidth saturates.
func BenchmarkBitmapConcurrency(b *testing.B) {
	sets := bench.BitmapSets(0xC0FFEE, 2, 100_000, bench.Sparse)
	left := MustFromSorted(sets[0])
	right := MustFromSorted(sets[1])
	var sink uint64
	for _, workers := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("Workers=%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			count := workers
			if count > b.N {
				count = b.N
			}
			per := b.N / count
			extra := b.N % count
			partial := make([]uint64, count)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for w := 0; w < count; w++ {
				n := per
				if w < extra {
					n++
				}
				wg.Add(1)
				go func(w, n int) {
					defer wg.Done()
					var sum uint64
					<-start
					for i := 0; i < n; i++ {
						sum += AndCardinality(left, right)
					}
					partial[w] = sum
				}(w, n)
			}
			b.ResetTimer()
			close(start)
			wg.Wait()
			for _, v := range partial {
				sink += v
			}
		})
	}
	concurrencySink += sink
}

var concurrencySink uint64
