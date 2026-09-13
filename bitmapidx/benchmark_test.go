package bitmapidx

import "testing"

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
