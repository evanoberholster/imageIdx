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
	for _, size := range []int{1, 16, 32, 64} {
		b.Run(fmt.Sprintf("batch-%d", size), func(b *testing.B) {
			data, idxPath := paths(b)
			db, err := OpenDirs(data, idxPath, decodeTest, Options{BatchSize: 64})
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

// BenchmarkCommitAndIndexWarm excludes the one-time coordinator workspace and
// first projection-block allocation from the measured steady state.
func BenchmarkCommitAndIndexWarm(b *testing.B) {
	for _, size := range []int{1, 16, 32, 64} {
		b.Run(fmt.Sprintf("batch-%d", size), func(b *testing.B) {
			data, idxPath := paths(b)
			db, err := OpenDirs(data, idxPath, decodeTest, Options{BatchSize: 64})
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			mutations := make([]Mutation, size)
			warm := func(base int) {
				for j := range mutations {
					id := uint64(base + j + 1)
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
			warm(0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				warm((i + 1) * size)
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "images/s")
		})
	}
}

func BenchmarkStartupRebuild(b *testing.B) {
	sizes := []int{10_000, 100_000, 1_000_000}
	if os.Getenv("IMAGEIDX_BENCH_LARGE") != "" {
		sizes = append(sizes, 10_000_000)
	}
	for _, total := range sizes {
		b.Run(fmt.Sprintf("records-%d", total), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				data, dir := paths(b)
				db, err := OpenDirs(data, dir, decodeTest, Options{BatchSize: 64})
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
				db, err = OpenDirs(data, dir, decodeTest, Options{BatchSize: 64, Recovery: RecoveryAutomatic})
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

func BenchmarkOpenExisting(b *testing.B) {
	data, idxPath := paths(b)
	db, err := OpenDirs(data, idxPath, decodeTest, Options{})
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db, err := OpenDirs(data, idxPath, decodeTest, Options{})
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkKVNamespaceAddition measures a startup-only additive change that
// introduces KV namespaces only, which must not trigger an index rebuild.
func BenchmarkKVNamespaceAddition(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		data, idxPath := paths(b)
		db, err := OpenDirs(data, idxPath, decodeTest, Options{})
		if err != nil {
			b.Fatal(err)
		}
		target := db.ids.Definitions()
		for n := 0; n < 4; n++ {
			if err := target.Register(schema.NamespaceDef{Namespace: schema.Namespace(700 + n), Name: fmt.Sprintf("extra-%d", n), Kind: schema.KindKV, Entity: schema.EntityImage}); err != nil {
				b.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		db, err = OpenDirs(data, idxPath, decodeTest, Options{Definitions: target})
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCanonicalProjectionModes(b *testing.B) {
	for _, external := range []bool{false, true} {
		name := "inline"
		if external {
			name = "external"
		}
		b.Run(name, func(b *testing.B) {
			data, idxPath := paths(b)
			options := Options{BatchSize: 64}
			if external {
				options.CanonicalProjection = CanonicalProjectionExternal
				options.ProjectFromRecord = func(record []byte) ([]byte, error) { return record, nil }
			}
			db, err := OpenDirs(data, idxPath, decodeTest, options)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			const size = 16
			mutations := make([]Mutation, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range mutations {
					id := uint64(i*size + j + 1)
					projection := projection(id, 'x')
					mutations[j] = Mutation{Entity: schema.EntityImage, ValueID: id, Projection: projection, Record: projection}
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
			stats := db.Stats()
			b.ReportMetric(float64(stats.Store.ValueBytes)/float64(max(b.N, 1)), "store-value-B/op")
			b.ReportMetric(float64(stats.JournalBytes)/float64(max(b.N, 1)), "journal-B/op")
		})
	}
}

func BenchmarkJournalEncodeReuse(b *testing.B) {
	before := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("before")}}}
	after := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("after")}}}
	m := Mutation{Entity: schema.EntityImage, ValueID: 1}
	var scratch []byte
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var err error
		scratch, err = appendMutation(scratch[:0], m, false, before, after)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJournalEncodeAlloc(b *testing.B) {
	before := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("before")}}}
	after := index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("after")}}}
	m := Mutation{Entity: schema.EntityImage, ValueID: 1}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := encodeMutation(m, false, before, after); err != nil {
			b.Fatal(err)
		}
	}
}
