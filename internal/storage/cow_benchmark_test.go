package storage

import (
	"fmt"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

func BenchmarkCOWExtentUnion(b *testing.B) {
	for _, count := range []int{400, 4000} {
		b.Run(fmt.Sprintf("Extents=%d", count), func(b *testing.B) {
			a, c := make([]Extent, count), make([]Extent, count)
			for n := range a {
				a[n] = Extent{Ref: pagealloc.Ref(2 + 2*n), Size: 512}
				c[count-1-n] = a[n]
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				live, err := UnionExtents(a, c)
				if err != nil || len(live) != count {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCOWAllocatorRebuild(b *testing.B) {
	for _, bytes := range []uint64{4 << 20, 1 << 30} {
		b.Run(fmt.Sprintf("ArenaBytes=%d", bytes), func(b *testing.B) {
			live := make([]Extent, 400)
			for n := range live {
				live[n] = Extent{Ref: pagealloc.Ref(2 + 8*n), Size: 4096}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				if _, err := ReconstructAllocator(bytes, live); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
