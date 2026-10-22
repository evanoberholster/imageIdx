package simhash

import (
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/bench"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func BenchmarkSparseScaleSearch(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			st, ids := sparseScaleStore(b, n)
			defer st.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := st.SearchPHash(nil, uint64(i%len(ids)+1), 0); len(got) != 1 {
					b.Fatalf("result = %+v", got)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(st.SparseSegmentCount()), "segments")
			b.ReportMetric(float64(bench.DiskBytes(st.layout.dir)), "disk-B")
		})
	}
}

func BenchmarkSparseScaleHotApply(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			st, ids := sparseScaleStore(b, n)
			defer st.Close()
			const batch = 64
			updates := make([]Update, batch)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range updates {
					id := ids[j%len(ids)]
					updates[j] = Update{ImageID: schema.ImageID(id), Record: Record{PHash: uint64(i + j)}}
				}
				if err := st.ApplyUpdates(updates); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(st.SparseSegmentCount()), "segments")
		})
	}
}

func sparseScaleStore(b *testing.B, n int) (*Store, []uint64) {
	b.Helper()
	st, err := Open(b.TempDir(), Config{Durability: schema.DurabilityOS})
	if err != nil {
		b.Fatal(err)
	}
	ids := bench.SparseIDs(n, 1<<40, 42)
	updates := make([]Update, 0, 64)
	for i, id := range ids {
		updates = append(updates, Update{ImageID: schema.ImageID(id), Record: Record{PHash: uint64(i + 1), PDQ: pdqForIndex(i)}})
		if len(updates) == 64 || i == len(ids)-1 {
			if err := st.ApplyUpdates(updates); err != nil {
				st.Close()
				b.Fatal(err)
			}
			updates = updates[:0]
		}
	}
	return st, ids
}

func BenchmarkSparseScaleReopen(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			st, _ := sparseScaleStore(b, n)
			dir := st.layout.dir
			if err := st.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				st, err := Open(dir, Config{Durability: schema.DurabilityOS})
				if err != nil {
					b.Fatal(err)
				}
				if got := st.SearchPHash(nil, 1, 0); len(got) != 1 {
					st.Close()
					b.Fatalf("result = %+v", got)
				}
				if err := st.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSparseScalePDQ(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			st, ids := sparseScaleStore(b, n)
			defer st.Close()
			query := pdqForIndex(0)
			b.Run("scalar", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if got := st.SearchPDQ(nil, &query, 0); len(got) != 1 || got[0].ImageID != schema.ImageID(ids[0]) {
						b.Fatalf("result = %+v", got)
					}
				}
			})
			b.Run("bulk", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if got := st.SearchPDQBulk(nil, &query, 0); len(got) != 1 || got[0].ImageID != schema.ImageID(ids[0]) {
						b.Fatalf("result = %+v", got)
					}
				}
			})
		})
	}
}

func pdqForIndex(i int) PDQHash {
	var out PDQHash
	binary.LittleEndian.PutUint64(out[:8], uint64(i+1))
	return out
}
