package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func BenchmarkCommitAndIndex(b *testing.B) {
	for _, size := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("batch-%d", size), func(b *testing.B) {
			data, idxPath := paths(b)
			db, err := Open(data, idxPath, decodeTest, Options{BatchSize: 64})
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			mutations := make([]Mutation, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range mutations {
					id := uint64(i*size + j + 1)
					mutations[j] = Mutation{Entity: 1, ValueID: id, Projection: projection(id, 'x')}
				}
				g, err := db.Commit(mutations...)
				if err != nil {
					b.Fatal(err)
				}
				if err := db.WaitIndexed(context.Background(), g); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "images/s")
		})
	}
}

func BenchmarkStartupRebuild(b *testing.B) {
	for _, total := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("records-%d", total), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				data, dir := paths(b)
				db, err := Open(data, dir, decodeTest, Options{BatchSize: 64})
				if err != nil {
					b.Fatal(err)
				}
				entries := make([]Mutation, 64)
				var generation uint64
				for first := 1; first <= total; first += len(entries) {
					n := min(len(entries), total-first+1)
					for j := range entries[:n] {
						id := uint64(first + j)
						entries[j] = Mutation{Entity: schema.EntityImage, ValueID: id, Projection: projection(id, 'a')}
					}
					generation, err = db.Commit(entries[:n]...)
					if err != nil {
						b.Fatal(err)
					}
				}
				if err := db.WaitIndexed(context.Background(), generation); err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				db, err = Open(data, dir, decodeTest, Options{BatchSize: 64, Recovery: RecoveryAutomatic})
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(total*b.N)/b.Elapsed().Seconds(), "records/s")
		})
	}
}

// BenchmarkGenerationPublication compares the previous two-step completeness
// path to the single-publication coordinator path on the same index schema.
func BenchmarkGenerationPublication(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "integrated"
		if legacy {
			name = "legacy"
		}
		b.Run(name, func(b *testing.B) {
			_, idxPath := paths(b)
			idx, err := index.Open(idxPath)
			if err != nil {
				b.Fatal(err)
			}
			defer idx.Close()
			batch := idx.NewBatch(1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := batch.Reset(); err != nil {
					b.Fatal(err)
				}
				if err := batch.Add(index.Entry{ValueID: uint64(i + 1), Bitmaps: []index.BitmapValue{{Namespace: tagNS, Value: bitmapidx.EnumStringValue("tag")}}}); err != nil {
					b.Fatal(err)
				}
				gen := uint64(i + 1)
				if legacy {
					if err := idx.ApplyWithGeneration(batch, gen); err != nil {
						b.Fatal(err)
					}
					if err := batch.Reset(); err != nil {
						b.Fatal(err)
					}
					if err := idx.ApplyWithGeneration(batch, gen); err != nil {
						b.Fatal(err)
					}
				} else {
					if err := idx.ApplyWithGenerationAll(batch, gen); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
