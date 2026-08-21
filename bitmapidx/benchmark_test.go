package bitmapidx

import "testing"

func BenchmarkContains(b *testing.B) {
	ids := make([]uint64, 100_000)
	for n := range ids {
		ids[n] = uint64(n * 3)
	}
	index, _ := New(BitmapKey{Type: IndexTypeTag, ValueID: 1}, 1, ids, Config{})
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = index.Contains(uint64(n % 300_000))
	}
}

func BenchmarkAnd4(b *testing.B) {
	indexes := make([]*BitmapIndex, 4)
	epoch := NewEpoch()
	for n := range indexes {
		ids := make([]uint64, 20_000)
		for j := range ids {
			ids[j] = uint64(j*(n+1) + n)
		}
		indexes[n], _ = New(BitmapKey{Type: IndexTypeTag, ValueID: uint64(n + 1)}, uint64(n+1), ids, Config{Epoch: epoch})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = And(indexes...)
	}
}

func BenchmarkWriteBatch1000(b *testing.B) {
	ids := make([]uint64, 1_000)
	for n := range ids {
		ids[n] = uint64(n)
	}
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		index, _ := New(BitmapKey{Type: IndexTypeTag, ValueID: 1}, 1, nil, Config{})
		_ = index.Add(ids)
		_ = index.Close()
	}
}
