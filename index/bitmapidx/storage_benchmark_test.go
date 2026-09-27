package bitmapidx

import (
	"fmt"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// seedStore builds a persistent store with values distinct enum values,
// each holding perValue images, adding in bounded batches. The newly added
// memberships stay in the WAL until the caller checkpoints.
func seedStore(b *testing.B, dir string, values, perValue int) *Store {
	b.Helper()
	s, err := NewStore(dir, []NamespaceSpec{enumNamespace(common.NamespaceTag)}, NamespaceConfig{}, common.DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	index, _ := s.Lookup(common.NamespaceTag)
	entries := make([]Entry, 0, 1024)
	flush := func() {
		if len(entries) == 0 {
			return
		}
		commit(b, index, func(bt *Batch) error { return bt.Add(entries...) })
		entries = entries[:0]
	}
	for value := 0; value < values; value++ {
		name := EnumStringValue(fmt.Sprintf("value-%d", value))
		for n := 0; n < perValue; n++ {
			entries = append(entries, Entry{ImageID: common.ImageID(value*perValue + n + 1), Value: name})
			if len(entries) == cap(entries) {
				flush()
			}
		}
	}
	flush()
	return s
}

// addTail appends updates memberships spread over distinct values, split
// into MaxNamespaceBatchUpdates-sized write batches.
func addTail(index *Index, baseImage common.ImageID, updates, distinct int) error {
	if updates <= 0 {
		return nil
	}
	batch := make([]Entry, 0, updates)
	for n := 0; n < updates; n++ {
		batch = append(batch, Entry{
			ImageID: baseImage + common.ImageID(n),
			Value:   EnumStringValue(fmt.Sprintf("tail-%d", n%distinct)),
		})
	}
	bt := index.NewBatch()
	defer bt.Close()
	for start := 0; start < len(batch); start += MaxNamespaceBatchUpdates {
		end := min(start+MaxNamespaceBatchUpdates, len(batch))
		if err := bt.Add(batch[start:end]...); err != nil {
			return err
		}
		if err := bt.Apply(); err != nil {
			return err
		}
		if err := index.Barrier(); err != nil {
			return err
		}
		if err := bt.Reset(); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkNamespaceCheckpoint measures the writer stall from rewriting and
// validating a full snapshot generation on checkpoint. It blocks all
// writers for the duration, so treat it as the write-stall budget probe.
func BenchmarkNamespaceCheckpoint(b *testing.B) {
	s := seedStore(b, b.TempDir(), 256, 256)
	b.Cleanup(func() { _ = s.Close() })
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := s.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNamespaceReopen measures restart time: manifest validation, snapshot
// open, and replay of a nonempty WAL tail. Target: reopening a tail of up to
// MaxNamespaceBatchUpdates should stay well under an interactive maintenance
// window (< 250 ms on reference hardware); this benchmark catches replay
// regressions as the format evolves.
func BenchmarkNamespaceReopen(b *testing.B) {
	const distinct = 64
	for _, updates := range []int{2048, 4096, 8192} {
		b.Run(fmt.Sprintf("updates=%d", updates), func(b *testing.B) {
			dir := b.TempDir()
			s := seedStore(b, dir, 256, 256)
			if err := s.Checkpoint(); err != nil {
				b.Fatal(err)
			}
			index, _ := s.Lookup(common.NamespaceTag)
			if err := addTail(index, 1_000_000, updates, distinct); err != nil {
				b.Fatal(err)
			}
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				opened, err := OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
				if err != nil {
					b.Fatal(err)
				}
				if err := opened.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkMaxCardinalityIndex(b *testing.B) *Index {
	b.Helper()
	index, err := NewIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	const ids = 1 << 18
	value := EnumStringValue("shared")
	entries := make([]Entry, 0, 4096)
	for n := 1; n <= ids; n++ {
		entries = append(entries, Entry{ImageID: common.ImageID(n), Value: value})
		if len(entries) == cap(entries) {
			commit(b, index, func(bt *Batch) error { return bt.Add(entries...) })
			entries = entries[:0]
		}
	}
	if len(entries) > 0 {
		commit(b, index, func(bt *Batch) error { return bt.Add(entries...) })
	}
	return index
}

// BenchmarkNamespaceMaxCardinalityMatchInMemory probes a single value holding
// many images on a standalone index, the worst case for intersecting and
// returning a posting with no persistence copy.
func BenchmarkNamespaceMaxCardinalityMatchInMemory(b *testing.B) {
	index := benchmarkMaxCardinalityIndex(b)
	value := EnumStringValue("shared")
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		if _, err := index.MatchValues(value); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNamespaceMaxCardinalityMatchPersistent adds the store-owned result
// copy: every match is cloned away from mmap state, so this is the number
// that matters for a persistent maximum-cardinality query.
func BenchmarkNamespaceMaxCardinalityMatchPersistent(b *testing.B) {
	s := seedStore(b, b.TempDir(), 1, 1<<18)
	b.Cleanup(func() { _ = s.Close() })
	if err := s.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	index, _ := s.Lookup(common.NamespaceTag)
	value := EnumStringValue("value-0")
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		if _, err := index.MatchValues(value); err != nil {
			b.Fatal(err)
		}
	}
}
