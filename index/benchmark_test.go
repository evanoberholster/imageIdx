package index

import (
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

func benchSchema(tb testing.TB) *common.Schema {
	tb.Helper()
	schema := common.NewSchema()
	if err := schema.Register(common.NamespaceDef{
		Namespace: 1, Name: "time", Kind: common.KindTimestamp, Entity: common.EntityImage,
	}); err != nil {
		tb.Fatal(err)
	}
	tags, err := BitmapDef(bitmapidx.NamespaceSpec{Namespace: 6, Kind: common.KindBitmapEnum}, "tags", common.EntityImage)
	if err != nil {
		tb.Fatal(err)
	}
	if err := schema.Register(tags); err != nil {
		tb.Fatal(err)
	}
	return schema
}

func benchStore(b *testing.B) *Store {
	b.Helper()
	db, err := Create(b.TempDir(), Spec{Schema: benchSchema(b)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

// BenchmarkStoreAdd measures the unified-layer overhead of one upsert over the
// engine work it drives.
func BenchmarkStoreAdd(b *testing.B) {
	db := benchStore(b)
	entry := Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 7}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Add(entry); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreApply measures batch mutation for the target 1-64 entity range.
func BenchmarkStoreApply(b *testing.B) {
	for _, size := range []int{1, 16, 32, 64} {
		b.Run(benchName(size), func(b *testing.B) {
			db := benchStore(b)
			batch := db.NewBatch(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := batch.Reset(); err != nil {
					b.Fatal(err)
				}
				for n := 0; n < size; n++ {
					if err := batch.Add(Entry{ValueID: common.ValueID(n + 1), Times: []TimeValue{{Namespace: 1, Timestamp: uint64(i + n)}}}); err != nil {
						b.Fatal(err)
					}
				}
				if err := db.Apply(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStoreUpdate measures a single partial change.
func BenchmarkStoreUpdate(b *testing.B) {
	db := benchStore(b)
	if err := db.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 1}}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Update(Change{
			Before: Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: uint64(i)}}},
			After:  Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: uint64(i + 1)}}},
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreDelete measures a single logical delete interleaved with a
// re-add so the entity is always present to delete.
func BenchmarkStoreDelete(b *testing.B) {
	db := benchStore(b)
	entry := Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 1}}}
	if err := db.Add(entry); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Delete(common.EntityImage, 1); err != nil {
			b.Fatal(err)
		}
		if err := db.Add(entry); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreApplyUpdate measures a batched partial update for the target
// 1-64 entity range.
func BenchmarkStoreApplyUpdate(b *testing.B) {
	for _, size := range []int{1, 16, 32, 64} {
		b.Run(benchName(size), func(b *testing.B) {
			db := benchStore(b)
			for n := 1; n <= size; n++ {
				if err := db.Add(Entry{ValueID: common.ValueID(n), Times: []TimeValue{{Namespace: 1, Timestamp: uint64(n)}}}); err != nil {
					b.Fatal(err)
				}
			}
			batch := db.NewBatch(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := batch.Reset(); err != nil {
					b.Fatal(err)
				}
				for n := 1; n <= size; n++ {
					if err := batch.Update(Change{
						Before: Entry{ValueID: common.ValueID(n), Times: []TimeValue{{Namespace: 1, Timestamp: uint64(i)}}},
						After:  Entry{ValueID: common.ValueID(n), Times: []TimeValue{{Namespace: 1, Timestamp: uint64(i + 1)}}},
					}); err != nil {
						b.Fatal(err)
					}
				}
				if err := db.Apply(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRegistryLookup verifies the routing table resolves without
// allocating. It is the hot namespace-resolution path used by every mutation
// and compiled query.
func BenchmarkRegistryLookup(b *testing.B) {
	db := benchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	var found bool
	for i := 0; i < b.N; i++ {
		_, found = db.reg.lookup(common.Namespace(i%8 + 1))
	}
	_ = found
}

func benchName(n int) string {
	switch n {
	case 1:
		return "Entities=1"
	case 16:
		return "Entities=16"
	case 32:
		return "Entities=32"
	default:
		return "Entities=64"
	}
}
