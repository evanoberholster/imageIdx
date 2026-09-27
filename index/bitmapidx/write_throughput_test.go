package bitmapidx

import (
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// Write-throughput probes for the Pebble-style commit comparison. Each
// benchmark writes fresh IDs so every operation performs real delta work
// (re-adding present memberships would collapse to no-ops). Every write
// pairs Apply with Barrier, which carries the same durability and
// visibility guarantee as the old synchronous Apply return.
func BenchmarkWriteSequentialSingle(b *testing.B) {
	index, err := NewNamespaceIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	value := StringValue("stream")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		batch := index.NewBatch()
		if err := batch.Add(Entry{ImageID: ImageID(n + 1), Value: value}); err != nil {
			b.Fatal(err)
		}
		if err := batch.Apply(); err != nil {
			b.Fatal(err)
		}
		batch.Close()
		if err := index.Barrier(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteSequentialBatch64(b *testing.B) {
	index, err := NewNamespaceIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	value := StringValue("stream")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		batch := index.NewBatch()
		for k := 0; k < 64; k++ {
			if err := batch.Add(Entry{ImageID: ImageID(n*64 + k + 1), Value: value}); err != nil {
				b.Fatal(err)
			}
		}
		if err := batch.Apply(); err != nil {
			b.Fatal(err)
		}
		batch.Close()
		if err := index.Barrier(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteSequentialBatch64Reused stages through one reused batch to
// show steady-state cost with buffer retention.
func BenchmarkWriteSequentialBatch64Reused(b *testing.B) {
	index, err := NewNamespaceIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	value := StringValue("stream")
	batch := index.NewBatch()
	defer batch.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := batch.Reset(); err != nil {
			b.Fatal(err)
		}
		for k := 0; k < 64; k++ {
			if err := batch.Add(Entry{ImageID: ImageID(n*64 + k + 1), Value: value}); err != nil {
				b.Fatal(err)
			}
		}
		if err := batch.Apply(); err != nil {
			b.Fatal(err)
		}
		if err := index.Barrier(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteParallelBatch16(b *testing.B) {
	index, err := NewNamespaceIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	value := StringValue("stream")
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			base := next.Add(16) - 16
			batch := index.NewBatch()
			for k := uint64(0); k < 16; k++ {
				if err := batch.Add(Entry{ImageID: ImageID(base + k + 1), Value: value}); err != nil {
					b.Error(err)
					return
				}
			}
			if err := batch.Apply(); err != nil {
				b.Error(err)
				return
			}
			batch.Close()
			if err := index.Barrier(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkWriteStoreSyncBatch16(b *testing.B) {
	s, err := NewIndex(b.TempDir(), []NamespaceSpec{enumNamespace(common.NamespaceTag)}, NamespaceConfig{}, DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	index, _ := s.Lookup(common.NamespaceTag)
	value := StringValue("stream")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		batch := index.NewBatch()
		for k := 0; k < 16; k++ {
			if err := batch.Add(Entry{ImageID: ImageID(n*16 + k + 1), Value: value}); err != nil {
				b.Fatal(err)
			}
		}
		if err := batch.Apply(); err != nil {
			b.Fatal(err)
		}
		batch.Close()
		if err := index.Barrier(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteStoreSyncParallelBatch16(b *testing.B) {
	s, err := NewIndex(b.TempDir(), []NamespaceSpec{enumNamespace(common.NamespaceTag)}, NamespaceConfig{}, DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	index, _ := s.Lookup(common.NamespaceTag)
	value := StringValue("stream")
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			base := next.Add(16) - 16
			batch := index.NewBatch()
			for k := uint64(0); k < 16; k++ {
				if err := batch.Add(Entry{ImageID: ImageID(base + k + 1), Value: value}); err != nil {
					b.Error(err)
					return
				}
			}
			if err := batch.Apply(); err != nil {
				b.Error(err)
				return
			}
			batch.Close()
			if err := index.Barrier(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
