package bitmapidx

import (
	"testing"

	"github.com/evanoberholster/imageIdx/roar"
)

func BenchmarkContains(b *testing.B) {
	ids := make([]uint64, 100_000)
	for n := range ids {
		ids[n] = uint64(n * 3)
	}
	index, _ := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: ids}, Config{})
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = index.Contains(uint64(n % 300_000))
	}
}

func BenchmarkAnd4(b *testing.B) {
	indexes := make([]*Index, 4)
	for n := range indexes {
		ids := make([]uint64, 20_000)
		for j := range ids {
			ids[j] = uint64(j*(n+1) + n)
		}
		indexes[n], _ = New(Spec{Key: Key{Type: IndexTypeTag, ValueID: uint64(n + 1)}, IndexID: uint64(n + 1), IDs: ids}, Config{})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = And(indexes...)
	}
}

func BenchmarkApplyBatch1000(b *testing.B) {
	ids := make([]uint64, 1_000)
	for n := range ids {
		ids[n] = uint64(n)
	}
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		index, _ := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1}, Config{})
		_ = index.Apply(Mutation{Add: ids})
		_ = index.Close()
	}
}

func BenchmarkAppendTo(b *testing.B) {
	ids := make([]uint64, 100_000)
	for n := range ids {
		ids[n] = uint64(n * 3)
	}
	index, _ := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: ids}, Config{})
	buffer := make([]ImageID, 0, 100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buffer = index.AppendTo(buffer[:0])
	}
}

func BenchmarkOr4(b *testing.B) {
	indexes := make([]*Index, 4)
	for n := range indexes {
		ids := make([]uint64, 20_000)
		for j := range ids {
			ids[j] = uint64(j*(n+1) + n)
		}
		indexes[n], _ = New(Spec{Key: Key{Type: IndexTypeTag, ValueID: uint64(n + 1)}, IndexID: uint64(n + 1), IDs: ids}, Config{})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = Or(indexes...)
	}
}

func BenchmarkAnd2(b *testing.B) {
	indexes := make([]*Index, 2)
	for n := range indexes {
		ids := make([]uint64, 20_000)
		for j := range ids {
			ids[j] = uint64(j*(n+1) + n)
		}
		indexes[n], _ = New(Spec{Key: Key{Type: IndexTypeTag, ValueID: uint64(n + 1)}, IndexID: uint64(n + 1), IDs: ids}, Config{})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = And(indexes...)
	}
}

func BenchmarkOr2(b *testing.B) {
	indexes := make([]*Index, 2)
	for n := range indexes {
		ids := make([]uint64, 20_000)
		for j := range ids {
			ids[j] = uint64(j*(n+1) + n)
		}
		indexes[n], _ = New(Spec{Key: Key{Type: IndexTypeTag, ValueID: uint64(n + 1)}, IndexID: uint64(n + 1), IDs: ids}, Config{})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = Or(indexes...)
	}
}

func BenchmarkFilter(b *testing.B) {
	indexes := make([]*Index, 4)
	for n := range indexes {
		ids := make([]uint64, 20_000)
		for j := range ids {
			ids[j] = uint64(j*(n+1) + n)
		}
		indexes[n], _ = New(Spec{Key: Key{Type: IndexTypeTag, ValueID: uint64(n + 1)}, IndexID: uint64(n + 1), IDs: ids}, Config{})
	}
	candidates := make([]ImageID, 10_000)
	for n := range candidates {
		candidates[n] = ImageID(n * 7)
	}
	buffer := make([]ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buffer = Filter(buffer[:0], candidates, indexes...)
	}
}

// roar backend primitives used by the v2 read and write paths.

// These benchmarks cover the roar set primitives bitmapidx relies on: build,
// membership, materialization, and the intersection/union/mutation patterns
// behind And, Or, and Apply.

func spacedIDs(count, step, shift int) []uint64 {
	ids := make([]uint64, count)
	for n := range ids {
		// +1 keeps every ID positive; roar reserves ID 0.
		ids[n] = uint64(n*step + shift + 1)
	}
	return ids
}

func benchmarkBackendBuild(b *testing.B) {
	ids := spacedIDs(100_000, 3, 0)
	b.ReportAllocs()
	for range b.N {
		_ = roar.FromUnsorted(ids)
	}
}

func benchmarkBackendContains(b *testing.B) {
	ids := spacedIDs(100_000, 3, 0)
	bitmap := roar.FromUnsorted(ids)
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		_ = bitmap.Contains(uint64(n % 300_000))
	}
}

func benchmarkBackendAppendTo(b *testing.B) {
	ids := spacedIDs(100_000, 3, 0)
	bitmap := roar.FromUnsorted(ids)
	dst := make([]uint64, 0, len(ids))
	b.ReportAllocs()
	for range b.N {
		dst = bitmap.AppendTo(dst[:0])
	}
}

func backendFourSets(count int) []roar.Bitmap {
	bitmaps := make([]roar.Bitmap, 4)
	for n := 0; n < 4; n++ {
		bitmaps[n] = roar.FromUnsorted(spacedIDs(count, n+1, n))
	}
	return bitmaps
}

func benchmarkBackendAnd4(b *testing.B) {
	bitmaps := backendFourSets(20_000)
	b.ReportAllocs()
	for range b.N {
		_ = roar.AndMany(bitmaps...)
	}
}

func benchmarkBackendOr4(b *testing.B) {
	bitmaps := backendFourSets(20_000)
	b.ReportAllocs()
	for range b.N {
		_ = roar.FastParOr(1, bitmaps...)
	}
}

func benchmarkBackendApply(b *testing.B) {
	base := spacedIDs(10_000, 3, 0)
	benchBackendApply(b, base, spacedIDs(1_000, 5, 1), spacedIDs(500, 6, 2))
}

func benchmarkBackendApplySmall(b *testing.B) {
	base := spacedIDs(10_000, 3, 0)
	benchBackendApply(b, base, spacedIDs(32, 5, 1), spacedIDs(16, 6, 2))
}

func benchBackendApply(b *testing.B, base, adds, removes []uint64) {
	baseBitmap := roar.FromUnsorted(base)
	b.ReportAllocs()
	for range b.N {
		builder := roar.NewDeltaBuilderFrom(baseBitmap)
		builder.AddMany(adds)
		for _, id := range removes {
			builder.Remove(id)
		}
		_ = builder.Freeze()
	}
}

func BenchmarkBackendBuild(b *testing.B)      { benchmarkBackendBuild(b) }
func BenchmarkBackendContains(b *testing.B)   { benchmarkBackendContains(b) }
func BenchmarkBackendAppendTo(b *testing.B)   { benchmarkBackendAppendTo(b) }
func BenchmarkBackendAnd4(b *testing.B)       { benchmarkBackendAnd4(b) }
func BenchmarkBackendOr4(b *testing.B)        { benchmarkBackendOr4(b) }
func BenchmarkBackendApply(b *testing.B)      { benchmarkBackendApply(b) }
func BenchmarkBackendApplySmall(b *testing.B) { benchmarkBackendApplySmall(b) }

// v2 apply batching: single updates versus one batched mutation.

func benchmarkIndexApplyN(b *testing.B, batch, total int) {
	ids := make([]ImageID, total)
	for i := range ids {
		ids[i] = ImageID(i + 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1}, Config{})
		if err != nil {
			b.Fatal(err)
		}
		for start := 0; start < total; start += batch {
			if err := index.Apply(Mutation{Add: ids[start : start+batch]}); err != nil {
				b.Fatal(err)
			}
		}
		_ = index.Close()
	}
}

func BenchmarkIndexApplySingle(b *testing.B) { benchmarkIndexApplyN(b, 1, 64) }
func BenchmarkIndexApplyBatch8(b *testing.B) { benchmarkIndexApplyN(b, 8, 64) }
