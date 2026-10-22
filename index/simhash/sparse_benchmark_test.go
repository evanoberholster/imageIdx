package simhash

import (
	"fmt"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func BenchmarkSparseApplyUpdates(b *testing.B) {
	for _, size := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("batch-%d", size), func(b *testing.B) {
			st, err := Open(b.TempDir(), Config{Durability: schema.DurabilityOS})
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			updates := make([]Update, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range updates {
					id := uint64(1<<32) + uint64(j+1)
					updates[j] = Update{ImageID: schema.ImageID(id), Record: Record{PHash: id}}
				}
				if err := st.ApplyUpdates(updates); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSparseSearchPHash(b *testing.B) {
	st, err := Open(b.TempDir(), Config{Durability: schema.DurabilityOS})
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	const count = 10000
	updates := make([]Update, count)
	for i := range updates {
		updates[i] = Update{ImageID: schema.ImageID(1<<32 + i + 1), Record: Record{PHash: uint64(i)}}
	}
	for start := 0; start < len(updates); start += 64 {
		end := min(start+64, len(updates))
		if err := st.ApplyUpdates(updates[start:end]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := st.SearchPHash(nil, uint64(i%count), 0); len(got) == 0 {
			b.Fatal("missing")
		}
	}
}

func BenchmarkSparseReopen(b *testing.B) {
	dir := b.TempDir()
	st, err := Open(dir, Config{Durability: schema.DurabilityOS})
	if err != nil {
		b.Fatal(err)
	}
	const count = 10000
	updates := make([]Update, count)
	for i := range updates {
		updates[i] = Update{ImageID: schema.ImageID(1<<32 + i + 1), Record: Record{PHash: uint64(i)}}
	}
	for start := 0; start < len(updates); start += 64 {
		end := min(start+64, len(updates))
		if err := st.ApplyUpdates(updates[start:end]); err != nil {
			b.Fatal(err)
		}
	}
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
		if err := st.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
