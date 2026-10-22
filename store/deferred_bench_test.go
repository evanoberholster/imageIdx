package store

import (
	"testing"

	"github.com/cockroachdb/pebble/v2"
)

func benchmarkDeferredStore(b *testing.B) *Store {
	b.Helper()
	s, err := OpenLegacy(b.TempDir(), Spec{Namespace: 100, Name: "deferred-bench"})
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkPebbleBatchSet(b *testing.B) {
	s := benchmarkDeferredStore(b)
	defer s.Close()
	var key [KeySize]byte
	value := make([]byte, 64)
	batch := s.db.NewBatch()
	defer batch.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch.Reset()
		for j := 0; j < 64; j++ {
			key[0], key[1] = 0, 100
			key[2], key[3] = byte(j>>8), byte(j)
			if err := batch.Set(key[:], value, nil); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkPebbleBatchSetDeferred(b *testing.B) {
	s := benchmarkDeferredStore(b)
	defer s.Close()
	var key [KeySize]byte
	value := make([]byte, 64)
	batch := s.db.NewBatch()
	defer batch.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch.Reset()
		for j := 0; j < 64; j++ {
			key[0], key[1] = 0, 100
			key[2], key[3] = byte(j>>8), byte(j)
			op := batch.SetDeferred(len(key), len(value))
			copy(op.Key, key[:])
			copy(op.Value, value)
			if err := op.Finish(); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkStoreBatchSet(b *testing.B) {
	s := benchmarkDeferredStore(b)
	defer s.Close()
	value := make([]byte, 64)
	batch := s.NewBatch()
	defer batch.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch.batch.Reset()
		for j := 0; j < 64; j++ {
			if err := batch.Set(Key{Namespace: 100, ID: uint64(j + 1)}, value); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkPebbleBatchSetDeferredCommit(b *testing.B) {
	s := benchmarkDeferredStore(b)
	defer s.Close()
	var key [KeySize]byte
	value := make([]byte, 64)
	batch := s.db.NewBatch()
	defer batch.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch.Reset()
		for j := 0; j < 64; j++ {
			key[0], key[1] = 0, 100
			key[2], key[3] = byte(j>>8), byte(j)
			op := batch.SetDeferred(len(key), len(value))
			copy(op.Key, key[:])
			copy(op.Value, value)
			if err := op.Finish(); err != nil {
				b.Fatal(err)
			}
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			b.Fatal(err)
		}
	}
}
