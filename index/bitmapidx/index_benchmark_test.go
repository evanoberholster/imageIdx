package bitmapidx

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
)

func benchmarkEnumNamespace(tb testing.TB, values int, perValue int) (*Index, []Value) {
	tb.Helper()
	index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		tb.Fatalf("NewMemory: %v", err)
	}
	tb.Cleanup(func() { _ = index.Close() })
	entries := make([]Entry, 0, values*perValue)
	names := make([]Value, values)
	for value := 0; value < values; value++ {
		name := fmt.Sprintf("value-%d", value)
		names[value] = EnumStringValue(name)
		for n := 0; n < perValue; n++ {
			entries = append(entries, Entry{ImageID: common.ImageID(value*perValue + n + 1), Value: names[value]})
		}
	}
	commit(tb, index, func(bt *Batch) error { return bt.Add(entries...) })
	return index, names
}

// benchmarkEnumIntersectionNamespace mirrors the legacy And4 fixture: each value
// has its own arithmetic ID set, and one image may carry all four values.
func benchmarkEnumIntersectionNamespace(tb testing.TB) (*Index, []Value) {
	tb.Helper()
	index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		tb.Fatalf("NewMemory: %v", err)
	}
	tb.Cleanup(func() { _ = index.Close() })
	const values, perValue = 4, 20_000
	names := make([]Value, values)
	entries := make([]Entry, 0, values*perValue)
	for value := 0; value < values; value++ {
		names[value] = EnumStringValue(fmt.Sprintf("value-%d", value))
		for n := 0; n < perValue; n++ {
			id := n*(value+1) + value
			if id == 0 { // ImageID zero is reserved by the namespace API.
				continue
			}
			entries = append(entries, Entry{ImageID: common.ImageID(id), Value: names[value]})
		}
	}
	commit(tb, index, func(bt *Batch) error { return bt.Add(entries...) })
	return index, names
}

func BenchmarkNamespaceContainsKey(b *testing.B) {
	index, _ := benchmarkEnumNamespace(b, 1, 100_000)
	key := hashCanonical(common.NamespaceTag, common.KindBitmapEnum, []byte("value-0"))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = index.ContainsKey(uint64(n%300_000), key)
	}
}

func BenchmarkNamespaceContainsValue(b *testing.B) {
	index, _ := benchmarkEnumNamespace(b, 1, 100_000)
	value := EnumBytesValue([]byte("value-0"))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, _ = index.Contains(uint64(n%300_000), value)
	}
}

func BenchmarkNamespaceMatch4(b *testing.B) {
	index, names := benchmarkEnumIntersectionNamespace(b)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := index.MatchValues(names...); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNamespaceApplyDistinct1000(b *testing.B) {
	entries := make([]Entry, 1_000)
	for n := range entries {
		entries[n] = Entry{ImageID: common.ImageID(n + 1), Value: EnumStringValue("value-" + strconv.Itoa(n))}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
		if err != nil {
			b.Fatal(err)
		}
		bt := index.NewBatch()
		if err := bt.Add(entries...); err != nil {
			b.Fatal(err)
		}
		if err := bt.Apply(); err != nil {
			b.Fatal(err)
		}
		if err := index.Barrier(); err != nil {
			b.Fatal(err)
		}
		_ = bt.Close()
		_ = index.Close()
	}
}

func BenchmarkNamespaceApplyBatch1000(b *testing.B) {
	entries := make([]Entry, 1_000)
	for n := range entries {
		entries[n] = Entry{ImageID: common.ImageID(n + 1), Value: EnumStringValue("batch")}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
		if err != nil {
			b.Fatal(err)
		}
		bt := index.NewBatch()
		if err := bt.Add(entries...); err != nil {
			b.Fatal(err)
		}
		if err := bt.Apply(); err != nil {
			b.Fatal(err)
		}
		if err := index.Barrier(); err != nil {
			b.Fatal(err)
		}
		_ = bt.Close()
		_ = index.Close()
	}
}

func BenchmarkNamespaceValueIDs(b *testing.B) {
	index, _ := benchmarkEnumNamespace(b, 1, 100_000)
	value := EnumStringValue("value-0")
	buffer := make([]common.ImageID, 0, 100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var err error
		buffer, err = index.ValueIDs(buffer[:0], value)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNamespaceRange(b *testing.B) {
	index, err := NewMemory(continuousNamespace(testFocalLength), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	entries := make([]Entry, 0, 100_000)
	for value := -5_000; value < 5_000; value++ {
		for n := 0; n < 10; n++ {
			id := common.ImageID((value+5_000)*10 + n + 1)
			entries = append(entries, Entry{ImageID: id, Value: IntValue(int64(value))})
		}
	}
	commit(b, index, func(bt *Batch) error { return bt.Add(entries...) })
	bounds := Between(-4_500, -3_500)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := index.Range(bounds); err != nil {
			b.Fatal(err)
		}
	}
}

// continuousEntries builds (distinct values) x (per value) memberships starting
// at grid value 0.
func continuousEntries(distinct, per int) []Entry {
	entries := make([]Entry, 0, distinct*per)
	for v := 0; v < distinct; v++ {
		for j := 0; j < per; j++ {
			entries = append(entries, Entry{ImageID: common.ImageID(v*per + j + 1), Value: IntValue(int64(v))})
		}
	}
	return entries
}

// benchmarkContinuousRangeIndex builds an in-memory continuous namespace with
// the given value shape.
func benchmarkContinuousRangeIndex(b *testing.B, distinct, per int) *Index {
	b.Helper()
	index, err := NewMemory(continuousNamespace(testFocalLength), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	bt := index.NewBatch()
	if err := bt.Add(continuousEntries(distinct, per)...); err != nil {
		b.Fatal(err)
	}
	if err := bt.Apply(); err != nil {
		b.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		b.Fatal(err)
	}
	return index
}

// BenchmarkNamespaceRangeShapes measures continuous union materialization
// across value shapes: sparse (many tiny postings), dense (few large postings),
// and mixed.
func BenchmarkNamespaceRangeShapes(b *testing.B) {
	datasets := []struct {
		name          string
		distinct, per int
	}{
		{"sparse-100k-1", 100_000, 1},
		{"dense-16-6250", 16, 6_250},
		{"mixed-5k-20", 5_000, 20},
	}
	for _, ds := range datasets {
		index := benchmarkContinuousRangeIndex(b, ds.distinct, ds.per)
		shapes := []struct {
			name   string
			lo, hi int64
		}{
			{"narrow", 0, int64(ds.distinct)/50 + 1},
			{"wide", 0, int64(ds.distinct)},
		}
		for _, shape := range shapes {
			b.Run(ds.name+"/"+shape.name, func(b *testing.B) {
				bounds := Between(shape.lo, shape.hi)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := index.Range(bounds); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkNamespaceRangeUnion isolates the multiway union across the number of
// in-range postings, for sparse, dense, and mixed value shapes.
func BenchmarkNamespaceRangeUnion(b *testing.B) {
	datasets := []struct {
		name          string
		distinct, per int
	}{
		{"sparse-1", 200_000, 1},
		{"dense-100", 1_000, 100},
		{"mixed-10", 20_000, 10},
	}
	spans := []int{2, 4, 8, 16, 32, 64, 256, 1024}
	for _, ds := range datasets {
		index := benchmarkContinuousRangeIndex(b, ds.distinct, ds.per)
		for _, span := range spans {
			if span > ds.distinct {
				continue
			}
			b.Run(ds.name+"-postings"+itoa(span), func(b *testing.B) {
				bounds := Between(0, int64(span))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := index.Range(bounds); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkNamespaceCountRange measures the count-only continuous path.
func BenchmarkNamespaceCountRange(b *testing.B) {
	index := benchmarkContinuousRangeIndex(b, 10_000, 10)
	shapes := []struct {
		name   string
		lo, hi int64
	}{
		{"narrow", 1_000, 2_000},
		{"wide", 0, 10_000},
	}
	for _, shape := range shapes {
		b.Run(shape.name, func(b *testing.B) {
			bounds := Between(shape.lo, shape.hi)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := index.CountRange(bounds); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNamespaceRangeDelta measures continuous ranges with a live active
// delta, where every in-range key must fold its corrections. Moves create both
// additions and removals across 256 distinct keys (the default delta bound).
func BenchmarkNamespaceRangeDelta(b *testing.B) {
	// Keep the delta in the visible layers: no synchronous fold, no rotation,
	// and a delayed background compaction so the timed loop sees stable deltas.
	config := NamespaceConfig{ActiveTarget: 1 << 20, HardDeltaLimit: 1 << 30, MaxDeltaValues: 1 << 20, CompactDelay: time.Hour}
	index, err := NewMemory(continuousNamespace(testFocalLength), config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	seed := index.NewBatch()
	if err := seed.Add(continuousEntries(10_000, 10)...); err != nil {
		b.Fatal(err)
	}
	if err := seed.Apply(); err != nil {
		b.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		b.Fatal(err)
	}
	// The delta corrects keys 1000..1255 with both additions and removals, so
	// the range [1000,1256] must fold a corrected posting for each one.
	const base = 1_000
	const keys = 256
	bt := index.NewBatch()
	moves := make([]Move, 0, keys*7)
	nextID := common.ImageID(1_000_000)
	for k := 0; k < keys; k++ {
		moves = append(moves, Move{ImageID: common.ImageID((base+k)*10 + 1), From: int64(base + k), To: int64(base + k + 50_000)})
		for j := 0; j < 6; j++ {
			moves = append(moves, Move{ImageID: nextID, From: 0, To: int64(base + k)})
			nextID++
		}
	}
	if err := bt.Move(moves...); err != nil {
		b.Fatal(err)
	}
	if err := bt.Apply(); err != nil {
		b.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		b.Fatal(err)
	}
	bounds := Between(base, base+keys)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := index.Range(bounds); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNamespaceFilter(b *testing.B) {
	index, names := benchmarkEnumIntersectionNamespace(b)
	candidates := make([]common.ImageID, 10_000)
	for n := range candidates {
		candidates[n] = common.ImageID(n*7 + 1)
	}
	buffer := make([]common.ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var err error
		buffer, err = index.FilterValues(buffer[:0], candidates, names...)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNamespaceTextMatch(b *testing.B) {
	index, err := NewMemory(textNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	replacements := make([]TextReplacement, 0, 10_000)
	for n := 0; n < 10_000; n++ {
		replacements = append(replacements, TextReplacement{
			ImageID: common.ImageID(n + 1),
			After:   []string{fmt.Sprintf("alpha beta gamma delta%d", n)},
		})
	}
	commit(b, index, func(bt *Batch) error { return bt.ReplaceText(replacements...) })
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := index.MatchText("alpha beta"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNamespaceHashValue(b *testing.B) {
	const cameraModel common.Namespace = 100
	value := []byte("Canon EOS R6 Mark II")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = hashCanonical(cameraModel, common.KindBitmapEnum, value)
	}
}

// Batch write shapes: single updates versus one batched mutation.

func batchEntries(total int) []Entry {
	entries := make([]Entry, total)
	for i := range entries {
		entries[i] = Entry{ImageID: common.ImageID(i + 1), Value: EnumStringValue("v")}
	}
	return entries
}

func benchmarkNamespaceApplyN(b *testing.B, batch, total int) {
	entries := batchEntries(total)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
		if err != nil {
			b.Fatal(err)
		}
		bt := index.NewBatch()
		for start := 0; start < total; start += batch {
			if err := bt.Add(entries[start : start+batch]...); err != nil {
				b.Fatal(err)
			}
			if err := bt.Apply(); err != nil {
				b.Fatal(err)
			}
			if err := index.Barrier(); err != nil {
				b.Fatal(err)
			}
			if err := bt.Reset(); err != nil {
				b.Fatal(err)
			}
		}
		_ = bt.Close()
		_ = index.Close()
	}
}

func BenchmarkNamespaceApplySingle(b *testing.B)  { benchmarkNamespaceApplyN(b, 1, 64) }
func BenchmarkNamespaceApplyBatch8(b *testing.B)  { benchmarkNamespaceApplyN(b, 8, 64) }
func BenchmarkNamespaceApplyBatch64(b *testing.B) { benchmarkNamespaceApplyN(b, 64, 64) }

func BenchmarkNamespaceApplyChannel8(b *testing.B) {
	entries := batchEntries(64)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
		if err != nil {
			b.Fatal(err)
		}
		bt := index.NewBatch()
		batches := make(chan []Entry, 8)
		done := make(chan struct{}, 8)
		go func() {
			defer bt.Close()
			for chunk := range batches {
				if err := bt.Add(chunk...); err != nil {
					b.Error(err)
				} else if err := bt.Apply(); err != nil {
					b.Error(err)
				} else if err := index.Barrier(); err != nil {
					b.Error(err)
				}
				if err := bt.Reset(); err != nil {
					b.Error(err)
				}
				done <- struct{}{}
			}
		}()
		for start := 0; start < 64; start += 8 {
			batches <- entries[start : start+8]
			<-done
		}
		close(batches)
		_ = index.Close()
	}
}

// Compiled queries resolve values once and execute repeatedly.

func BenchmarkCompiledMatch4(b *testing.B) {
	index, names := benchmarkEnumIntersectionNamespace(b)
	compiled, err := index.Compile(names...)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := compiled.Match(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompiledCount4(b *testing.B) {
	index, names := benchmarkEnumNamespace(b, 4, 20_000)
	compiled, err := index.Compile(names...)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := compiled.Count(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompiledFilter(b *testing.B) {
	index, names := benchmarkEnumIntersectionNamespace(b)
	compiled, err := index.Compile(names...)
	if err != nil {
		b.Fatal(err)
	}
	candidates := make([]common.ImageID, 10_000)
	for n := range candidates {
		candidates[n] = common.ImageID(n*7 + 1)
	}
	buffer := make([]common.ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var err error
		buffer, err = compiled.Filter(buffer[:0], candidates)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompiledTextMatch(b *testing.B) {
	index, err := NewMemory(textNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	replacements := make([]TextReplacement, 0, 10_000)
	for n := 0; n < 10_000; n++ {
		replacements = append(replacements, TextReplacement{
			ImageID: common.ImageID(n + 1),
			After:   []string{fmt.Sprintf("alpha beta gamma delta%d", n)},
		})
	}
	commit(b, index, func(bt *Batch) error { return bt.ReplaceText(replacements...) })
	compiled, err := index.CompileText("alpha beta")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := compiled.Match(); err != nil {
			b.Fatal(err)
		}
	}
}

// Container transitions exercise postingTable folds between the tiny
// inline form and roaring bitmaps.

var sinkTable postingTable

// seededEnumNamespace builds a compacted enum base with count members under one
// value and returns the namespace, the value key, and the Value.
func seededEnumNamespace(b *testing.B, count int) (*Index, NamespaceKey, Value) {
	b.Helper()
	value := EnumStringValue("v")
	namespace, err := NewMemory(enumNamespace(common.NamespaceTag), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = namespace.Close() })
	for id := 1; id <= count; id++ {
		commit(b, namespace, func(bt *Batch) error {
			return bt.Add(Entry{ImageID: common.ImageID(id), Value: value})
		})
	}
	if err := namespace.Compact(); err != nil {
		b.Fatal(err)
	}
	return namespace, hashCanonical(common.NamespaceTag, common.KindBitmapEnum, []byte("v")), value
}

func testDelta(key NamespaceKey, add, remove []common.ImageID) *namespaceDelta {
	bd := newBitmapDelta(add, remove)
	if bd == nil {
		return nil
	}
	return &namespaceDelta{values: map[NamespaceKey]*bitmapDelta{key: bd}, keys: []NamespaceKey{key}, ids: bd.len()}
}

// BenchmarkContainerFold isolates the fold that changes container types:
// tiny->tiny, tiny->bitmap (promotion), bitmap->bitmap, bitmap->tiny
// (demotion), and key removal.
func BenchmarkContainerFold(b *testing.B) {
	cases := []struct {
		name   string
		base   int
		add    []common.ImageID
		remove []common.ImageID
	}{
		{"TinyAdd", 3, []common.ImageID{1000}, nil},
		{"TinyPromote", 4, []common.ImageID{1000}, nil},
		{"TinyDrop", 1, nil, []common.ImageID{1}},
		{"BitmapAdd", 64, []common.ImageID{1000}, nil},
		{"BitmapDemote", 5, nil, []common.ImageID{1}},
		{"BitmapDrop", 5, nil, []common.ImageID{1, 2, 3, 4, 5}},
	}
	for _, testCase := range cases {
		b.Run(testCase.name, func(b *testing.B) {
			namespace, key, _ := seededEnumNamespace(b, testCase.base)
			view := namespace.view.Load()
			delta := testDelta(key, testCase.add, testCase.remove)
			if delta == nil {
				b.Fatal("empty delta")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				sinkTable = view.effectiveTable(delta)
			}
		})
	}
}

// BenchmarkContainerDelta measures the public Add+Remove cycle while the write
// stays in the Active delta (no fold), for a tiny and a bitmap base posting.
func BenchmarkContainerDelta(b *testing.B) {
	for _, count := range []int{3, 64} {
		b.Run(fmt.Sprintf("Base%d", count), func(b *testing.B) {
			namespace, _, value := seededEnumNamespace(b, count)
			batch := namespace.NewBatch()
			defer batch.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				id := common.ImageID(10_000 + (n & 1023))
				if err := batch.Reset(); err != nil {
					b.Fatal(err)
				}
				if err := batch.Add(Entry{ImageID: id, Value: value}); err != nil {
					b.Fatal(err)
				}
				if err := batch.Apply(); err != nil {
					b.Fatal(err)
				}
				if err := namespace.Barrier(); err != nil {
					b.Fatal(err)
				}
				if err := batch.Reset(); err != nil {
					b.Fatal(err)
				}
				if err := batch.Remove(Entry{ImageID: id, Value: value}); err != nil {
					b.Fatal(err)
				}
				if err := batch.Apply(); err != nil {
					b.Fatal(err)
				}
				if err := namespace.Barrier(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkContainerFlip measures the full public path for a tiny<->bitmap
// promotion and demotion cycle, including publication and both folds.
func BenchmarkContainerFlip(b *testing.B) {
	b.Run("Enum", func(b *testing.B) {
		namespace, _, value := seededEnumNamespace(b, 4)
		batch := namespace.NewBatch()
		defer batch.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if err := batch.Reset(); err != nil {
				b.Fatal(err)
			}
			if err := batch.Add(Entry{ImageID: 1000, Value: value}); err != nil {
				b.Fatal(err)
			}
			if err := batch.Apply(); err != nil {
				b.Fatal(err)
			}
			if err := namespace.Barrier(); err != nil {
				b.Fatal(err)
			}
			if err := namespace.Compact(); err != nil {
				b.Fatal(err)
			}
			if err := batch.Reset(); err != nil {
				b.Fatal(err)
			}
			if err := batch.Remove(Entry{ImageID: 1000, Value: value}); err != nil {
				b.Fatal(err)
			}
			if err := batch.Apply(); err != nil {
				b.Fatal(err)
			}
			if err := namespace.Barrier(); err != nil {
				b.Fatal(err)
			}
			if err := namespace.Compact(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
