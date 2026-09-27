package bitmapidx

import (
	"fmt"
	"math"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/bench"
	"github.com/evanoberholster/imageIdx/roar"
)

// NamespaceSpec-index query matrix. Each dataset size N builds one immutable enum
// namespace with D = N/16 distinct values and 16 memberships per value. IDs come
// from bench.BitmapIDs and value assignment from a seeded permutation, so
// every run is reproducible and benchstat-comparable. Construction, sampling,
// and buffer allocation stay outside every timed region (Bench.MD 4, 23).
const (
	matrixSeed         int64 = 0x1dea5eed
	matrixPerValue     int   = 16
	matrixSampleSize   int   = 1_024
	matrixCandidateMax int   = 10_000
)

// Package-level sinks stop the compiler from eliminating measured work
// (Bench.MD 22).
var (
	sinkNamespaceBitmap   roar.Bitmap
	sinkNamespaceIDs      []common.ImageID
	sinkNamespaceFiltered []common.ImageID
	sinkNamespaceAppended []common.ImageID
	sinkNamespaceBool     bool
	sinkNamespaceCount    uint64
)

// namespaceMatrix is one seeded enum fixture shared by all query benchmarks of a
// dataset size. The caller owns index and must close it.
type namespaceMatrix struct {
	index      *Index
	ids        []uint64
	values     []Value
	hits       []Entry
	misses     []Entry
	candidates []common.ImageID
}

// buildNamespaceMatrix seeds the fixture described above. hits/misses are sampled
// entries with matching and deliberately wrong values; candidates is a
// deterministic sparse-ordered sample of indexed IDs for filter benchmarks.
func buildNamespaceMatrix(tb testing.TB, n int) *namespaceMatrix {
	tb.Helper()
	if n < 1 {
		n = 1
	}
	distinct := n / matrixPerValue
	if distinct < 1 {
		distinct = 1
	}
	ids := bench.BitmapIDs(matrixSeed, n, bench.Sparse)
	perm := bench.NewRNG(matrixSeed + 1).Perm(n)
	values := make([]Value, distinct)
	for i := range values {
		values[i] = EnumStringValue(fmt.Sprintf("value-%d", i))
	}
	index, err := NewIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		tb.Fatalf("NewIndex: %v", err)
	}
	matrix := &namespaceMatrix{index: index, ids: ids, values: values}
	matrix.hits = make([]Entry, 0, min(n, matrixSampleSize))
	matrix.misses = make([]Entry, 0, min(n, matrixSampleSize))
	absentValue := EnumStringValue("__absent__")
	entries := make([]Entry, 0, MaxNamespaceBatchUpdates)
	for i, id := range ids {
		valueIndex := perm[i] % distinct
		entry := Entry{ImageID: common.ImageID(id), Value: values[valueIndex]}
		entries = append(entries, entry)
		if len(matrix.hits) < matrixSampleSize {
			missValue := absentValue
			if distinct > 1 {
				missValue = values[(valueIndex+1)%distinct]
			}
			matrix.hits = append(matrix.hits, entry)
			matrix.misses = append(matrix.misses, Entry{
				ImageID: entry.ImageID,
				Value:   missValue,
			})
		}
		if len(entries) == cap(entries) {
			commit(tb, index, func(batch *Batch) error { return batch.Add(entries...) })
			entries = entries[:0]
		}
	}
	if len(entries) > 0 {
		commit(tb, index, func(batch *Batch) error { return batch.Add(entries...) })
	}
	if err := index.Compact(); err != nil {
		tb.Fatalf("Compact: %v", err)
	}
	if got := index.Cardinality(); got != uint64(distinct) {
		tb.Fatalf("Cardinality = %d, want %d", got, distinct)
	}
	count := min(n, matrixCandidateMax)
	picks := bench.Workload(matrixSeed+2, bench.Uniform, n, count)
	matrix.candidates = make([]common.ImageID, count)
	for i, pick := range picks {
		matrix.candidates[i] = common.ImageID(ids[pick])
	}
	return matrix
}

func BenchmarkNamespaceIndex(b *testing.B) {
	benchmarkNamespaceMatrixAdversarial(b)
	for _, size := range bench.Sizes(b) {
		benchmarkNamespaceMatrixSize(b, size)
	}
}

func benchmarkNamespaceMatrixSize(b *testing.B, n int) {
	heapBefore := bench.HeapBytes()
	matrix := buildNamespaceMatrix(b, n)
	defer func() { _ = matrix.index.Close() }()
	heapAfter := bench.HeapBytes()
	bench.ReportIndexSize(b, fmt.Sprintf("Index/N=%d", n), heapBefore, heapAfter, 0, n)

	for _, k := range []int{1, 2, 4} {
		if k > len(matrix.values) {
			break
		}
		b.Run(fmt.Sprintf("MatchValues/K=%d/N=%d", k, n), func(b *testing.B) {
			query := matrix.values[:k]
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result, err := matrix.index.MatchValues(query...)
				if err != nil {
					b.Fatal(err)
				}
				sinkNamespaceBitmap = result
			}
		})
	}

	b.Run(fmt.Sprintf("Contains/Hit/N=%d", n), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			entry := matrix.hits[i%len(matrix.hits)]
			present, err := matrix.index.Contains(entry.ImageID, entry.Value)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})
	b.Run(fmt.Sprintf("Contains/Miss/N=%d", n), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			entry := matrix.misses[i%len(matrix.misses)]
			present, err := matrix.index.Contains(entry.ImageID, entry.Value)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})

	b.Run(fmt.Sprintf("ValueIDs/N=%d", n), func(b *testing.B) {
		buffer := make([]common.ImageID, 0, matrixPerValue)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var err error
			buffer, err = matrix.index.ValueIDs(buffer[:0], matrix.values[0])
			if err != nil {
				b.Fatal(err)
			}
		}
		sinkNamespaceIDs = buffer
	})

	for _, k := range []int{1, 2} {
		if k > len(matrix.values) {
			break
		}
		b.Run(fmt.Sprintf("FilterValues/K=%d/N=%d/Dst=Preallocated", k, n), func(b *testing.B) {
			query := matrix.values[:k]
			dst := make([]common.ImageID, 0, len(matrix.candidates))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var err error
				dst, err = matrix.index.FilterValues(dst[:0], matrix.candidates, query...)
				if err != nil {
					b.Fatal(err)
				}
			}
			sinkNamespaceFiltered = dst
		})
		b.Run(fmt.Sprintf("FilterValues/K=%d/N=%d/Dst=Growing", k, n), func(b *testing.B) {
			query := matrix.values[:k]
			var dst []common.ImageID
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var err error
				dst, err = matrix.index.FilterValues(nil, matrix.candidates, query...)
				if err != nil {
					b.Fatal(err)
				}
			}
			sinkNamespaceFiltered = dst
		})
	}

	b.Run(fmt.Sprintf("Cardinality/N=%d", n), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			sinkNamespaceCount = matrix.index.Cardinality()
		}
	})

	absent := EnumStringValue("__absent__")
	b.Run(fmt.Sprintf("Adversarial/UnknownValue/N=%d/MatchValues", n), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			result, err := matrix.index.MatchValues(absent)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBitmap = result
		}
	})
	b.Run(fmt.Sprintf("Adversarial/UnknownValue/N=%d/Contains", n), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			present, err := matrix.index.Contains(matrix.hits[0].ImageID, absent)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})
	b.Run(fmt.Sprintf("Adversarial/UnknownValue/N=%d/ValueIDs", n), func(b *testing.B) {
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var err error
			dst, err = matrix.index.ValueIDs(dst[:0], absent)
			if err != nil {
				b.Fatal(err)
			}
		}
		sinkNamespaceIDs = dst
	})
	b.Run(fmt.Sprintf("Adversarial/AppendTo/N=%d", n), func(b *testing.B) {
		dst := make([]common.ImageID, 0, n)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			dst = matrix.index.AppendTo(dst[:0])
		}
		sinkNamespaceAppended = dst
	})
}

// benchmarkNamespaceMatrixAdversarial covers the hostile shapes from Bench.MD 19
// that do not depend on a dataset size: empty index, single entry, and a
// membership at math.MaxUint64.
func benchmarkNamespaceMatrixAdversarial(b *testing.B) {
	empty, err := NewIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = empty.Close() }()

	absent := EnumStringValue("absent")
	b.Run("Adversarial/EmptyIndex/MatchValues", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			result, err := empty.MatchValues(absent)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBitmap = result
		}
	})
	b.Run("Adversarial/EmptyIndex/Contains", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			present, err := empty.Contains(1, absent)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})
	b.Run("Adversarial/EmptyIndex/ValueIDs", func(b *testing.B) {
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var err error
			dst, err = empty.ValueIDs(dst[:0], absent)
			if err != nil {
				b.Fatal(err)
			}
		}
		sinkNamespaceIDs = dst
	})
	b.Run("Adversarial/EmptyIndex/FilterValues", func(b *testing.B) {
		candidates := make([]common.ImageID, 0, 64)
		for id := common.ImageID(1); id <= 64; id++ {
			candidates = append(candidates, id)
		}
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var err error
			dst, err = empty.FilterValues(dst[:0], candidates, absent)
			if err != nil {
				b.Fatal(err)
			}
		}
		sinkNamespaceFiltered = dst
	})
	b.Run("Adversarial/EmptyIndex/Cardinality", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			sinkNamespaceCount = empty.Cardinality()
		}
	})
	b.Run("Adversarial/EmptyIndex/AppendTo", func(b *testing.B) {
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			dst = empty.AppendTo(dst[:0])
		}
		sinkNamespaceAppended = dst
	})

	single, err := NewIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = single.Close() }()
	only := EnumStringValue("only")
	commit(b, single, func(batch *Batch) error {
		return batch.Add(Entry{ImageID: 1, Value: only})
	})
	b.Run("Adversarial/SingleEntry/MatchValues", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			result, err := single.MatchValues(only)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBitmap = result
		}
	})
	b.Run("Adversarial/SingleEntry/Contains/Hit", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			present, err := single.Contains(1, only)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})
	b.Run("Adversarial/SingleEntry/Contains/Miss", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			present, err := single.Contains(2, only)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})
	b.Run("Adversarial/SingleEntry/AppendTo", func(b *testing.B) {
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			dst = single.AppendTo(dst[:0])
		}
		sinkNamespaceAppended = dst
	})

	maxNamespace, err := NewIndex(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = maxNamespace.Close() }()
	maxValue := EnumStringValue("max")
	maxID := common.ImageID(math.MaxUint64)
	commit(b, maxNamespace, func(batch *Batch) error {
		return batch.Add(Entry{ImageID: maxID, Value: maxValue})
	})
	if present, err := maxNamespace.Contains(maxID, maxValue); err != nil || !present {
		b.Fatalf("max ImageID membership missing: present=%v err=%v", present, err)
	}
	if got := maxNamespace.AppendTo(nil); len(got) != 1 || got[0] != maxID {
		b.Fatalf("max ImageID AppendTo = %v, want [%d]", got, maxID)
	}
	b.Run("Adversarial/MaxImageID/Contains", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			present, err := maxNamespace.Contains(maxID, maxValue)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBool = present
		}
	})
	b.Run("Adversarial/MaxImageID/MatchValues", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			result, err := maxNamespace.MatchValues(maxValue)
			if err != nil {
				b.Fatal(err)
			}
			sinkNamespaceBitmap = result
		}
	})
	b.Run("Adversarial/MaxImageID/ValueIDs", func(b *testing.B) {
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var err error
			dst, err = maxNamespace.ValueIDs(dst[:0], maxValue)
			if err != nil {
				b.Fatal(err)
			}
		}
		sinkNamespaceIDs = dst
	})
	b.Run("Adversarial/MaxImageID/AppendTo", func(b *testing.B) {
		var dst []common.ImageID
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			dst = maxNamespace.AppendTo(dst[:0])
		}
		sinkNamespaceAppended = dst
	})
}
