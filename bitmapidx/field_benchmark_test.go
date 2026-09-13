package bitmapidx

import (
	"fmt"
	"strconv"
	"testing"
)

func benchmarkEnumField(tb testing.TB, values int, perValue int) (*FieldIndex, []Value) {
	tb.Helper()
	index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
	if err != nil {
		tb.Fatalf("NewFieldIndex: %v", err)
	}
	tb.Cleanup(func() { _ = index.Close() })
	entries := make([]Entry, 0, values*perValue)
	names := make([]Value, values)
	for value := 0; value < values; value++ {
		name := fmt.Sprintf("value-%d", value)
		names[value] = StringValue(name)
		for n := 0; n < perValue; n++ {
			entries = append(entries, Entry{ImageID: ImageID(value*perValue + n + 1), Value: names[value]})
		}
	}
	if err := index.Add(entries...); err != nil {
		tb.Fatalf("Add: %v", err)
	}
	return index, names
}

func BenchmarkFieldContainsKey(b *testing.B) {
	index, _ := benchmarkEnumField(b, 1, 100_000)
	key := hashCanonical(IndexTypeTag, KindEnum, []byte("value-0"))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = index.ContainsKey(uint64(n%300_000), key)
	}
}

func BenchmarkFieldContainsValue(b *testing.B) {
	index, _ := benchmarkEnumField(b, 1, 100_000)
	value := BytesValue([]byte("value-0"))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, _ = index.Contains(uint64(n%300_000), value)
	}
}

func BenchmarkFieldMatch4(b *testing.B) {
	index, names := benchmarkEnumField(b, 4, 20_000)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := index.MatchValues(names...); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFieldApplyDistinct1000(b *testing.B) {
	entries := make([]Entry, 1_000)
	for n := range entries {
		entries[n] = Entry{ImageID: ImageID(n + 1), Value: StringValue("value-" + strconv.Itoa(n))}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
		if err != nil {
			b.Fatal(err)
		}
		if err := index.Apply(FieldMutation{Add: entries}); err != nil {
			b.Fatal(err)
		}
		_ = index.Close()
	}
}

func BenchmarkFieldApplyBatch1000(b *testing.B) {
	entries := make([]Entry, 1_000)
	for n := range entries {
		entries[n] = Entry{ImageID: ImageID(n + 1), Value: StringValue("batch")}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
		if err != nil {
			b.Fatal(err)
		}
		if err := index.Apply(FieldMutation{Add: entries}); err != nil {
			b.Fatal(err)
		}
		_ = index.Close()
	}
}

func BenchmarkFieldValueIDs(b *testing.B) {
	index, _ := benchmarkEnumField(b, 1, 100_000)
	value := StringValue("value-0")
	buffer := make([]ImageID, 0, 100_000)
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

func BenchmarkFieldRange(b *testing.B) {
	index, err := NewFieldIndex(continuousField(testFocalLength), FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	entries := make([]Entry, 0, 100_000)
	for value := -5_000; value < 5_000; value++ {
		for n := 0; n < 10; n++ {
			id := ImageID((value+5_000)*10 + n + 1)
			entries = append(entries, Entry{ImageID: id, Value: IntValue(int64(value))})
		}
	}
	if err := index.Add(entries...); err != nil {
		b.Fatal(err)
	}
	bounds := Between(-4_500, -3_500)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := index.Range(bounds); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFieldFilter(b *testing.B) {
	index, names := benchmarkEnumField(b, 4, 20_000)
	candidates := make([]ImageID, 10_000)
	for n := range candidates {
		candidates[n] = ImageID(n*7 + 1)
	}
	buffer := make([]ImageID, 0, 10_000)
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

func BenchmarkFieldTextMatch(b *testing.B) {
	index, err := NewFieldIndex(textField(IndexTypeTag), FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	replacements := make([]TextReplacement, 0, 10_000)
	for n := 0; n < 10_000; n++ {
		replacements = append(replacements, TextReplacement{
			ImageID: ImageID(n + 1),
			After:   []string{fmt.Sprintf("alpha beta gamma delta%d", n)},
		})
	}
	if err := index.ReplaceText(replacements...); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := index.MatchText("alpha beta"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFieldHashValue(b *testing.B) {
	const cameraModel IndexType = 100
	value := []byte("Canon EOS R6 Mark II")
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = hashCanonical(cameraModel, KindEnum, value)
	}
}

// Batch write shapes: single updates versus one batched mutation.

func batchEntries(total int) []Entry {
	entries := make([]Entry, total)
	for i := range entries {
		entries[i] = Entry{ImageID: ImageID(i + 1), Value: StringValue("v")}
	}
	return entries
}

func benchmarkFieldApplyN(b *testing.B, batch, total int) {
	entries := batchEntries(total)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
		if err != nil {
			b.Fatal(err)
		}
		for start := 0; start < total; start += batch {
			if err := index.Apply(FieldMutation{Add: entries[start : start+batch]}); err != nil {
				b.Fatal(err)
			}
		}
		_ = index.Close()
	}
}

func BenchmarkFieldApplySingle(b *testing.B)  { benchmarkFieldApplyN(b, 1, 64) }
func BenchmarkFieldApplyBatch8(b *testing.B)  { benchmarkFieldApplyN(b, 8, 64) }
func BenchmarkFieldApplyBatch64(b *testing.B) { benchmarkFieldApplyN(b, 64, 64) }

func BenchmarkFieldApplyChannel8(b *testing.B) {
	entries := batchEntries(64)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
		if err != nil {
			b.Fatal(err)
		}
		batches := make(chan []Entry, 8)
		done := make(chan struct{}, 8)
		go func() {
			for batch := range batches {
				if err := index.Apply(FieldMutation{Add: batch}); err != nil {
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
	index, names := benchmarkEnumField(b, 4, 20_000)
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
	index, names := benchmarkEnumField(b, 4, 20_000)
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
	index, names := benchmarkEnumField(b, 4, 20_000)
	compiled, err := index.Compile(names...)
	if err != nil {
		b.Fatal(err)
	}
	candidates := make([]ImageID, 10_000)
	for n := range candidates {
		candidates[n] = ImageID(n*7 + 1)
	}
	buffer := make([]ImageID, 0, 10_000)
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
	index, err := NewFieldIndex(textField(IndexTypeTag), FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	replacements := make([]TextReplacement, 0, 10_000)
	for n := 0; n < 10_000; n++ {
		replacements = append(replacements, TextReplacement{
			ImageID: ImageID(n + 1),
			After:   []string{fmt.Sprintf("alpha beta gamma delta%d", n)},
		})
	}
	if err := index.ReplaceText(replacements...); err != nil {
		b.Fatal(err)
	}
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

// seededEnumField builds a compacted enum base with count members under one
// value and returns the field, the value key, and the Value.
func seededEnumField(b *testing.B, count int) (*FieldIndex, FieldKey, Value) {
	b.Helper()
	value := StringValue("v")
	field, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = field.Close() })
	for id := 1; id <= count; id++ {
		if err := field.Add(Entry{ImageID: ImageID(id), Value: value}); err != nil {
			b.Fatal(err)
		}
	}
	if err := field.Compact(); err != nil {
		b.Fatal(err)
	}
	return field, hashCanonical(IndexTypeTag, KindEnum, []byte("v")), value
}

func testDelta(key FieldKey, add, remove []ImageID) *fieldDelta {
	bd := newBitmapDelta(add, remove)
	if bd == nil {
		return nil
	}
	return &fieldDelta{values: map[FieldKey]*bitmapDelta{key: bd}, keys: []FieldKey{key}, ids: bd.len()}
}

// BenchmarkContainerFold isolates the fold that changes container types:
// tiny->tiny, tiny->bitmap (promotion), bitmap->bitmap, bitmap->tiny
// (demotion), and key removal.
func BenchmarkContainerFold(b *testing.B) {
	cases := []struct {
		name   string
		base   int
		add    []ImageID
		remove []ImageID
	}{
		{"TinyAdd", 3, []ImageID{1000}, nil},
		{"TinyPromote", 4, []ImageID{1000}, nil},
		{"TinyDrop", 1, nil, []ImageID{1}},
		{"BitmapAdd", 64, []ImageID{1000}, nil},
		{"BitmapDemote", 5, nil, []ImageID{1}},
		{"BitmapDrop", 5, nil, []ImageID{1, 2, 3, 4, 5}},
	}
	for _, testCase := range cases {
		b.Run(testCase.name, func(b *testing.B) {
			field, key, _ := seededEnumField(b, testCase.base)
			view := field.view.Load()
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
			field, _, value := seededEnumField(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				id := ImageID(10_000 + (n & 1023))
				if err := field.Add(Entry{ImageID: id, Value: value}); err != nil {
					b.Fatal(err)
				}
				if err := field.Remove(Entry{ImageID: id, Value: value}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkContainerGeoMove measures one Add+Remove transition cycle between
// two geo buckets of the given container sizes. The round trip returns the
// moved image to its source bucket.
func BenchmarkContainerGeoMove(b *testing.B) {
	paris, err := KeyFromLatLng(48.8566, 2.3522)
	if err != nil {
		b.Fatal(err)
	}
	london, err := KeyFromLatLng(51.5074, -0.1278)
	if err != nil {
		b.Fatal(err)
	}
	cases := []struct{ from, to int }{
		{3, 3},
		{4, 4},
		{4, 64},
		{64, 4},
		{64, 64},
	}
	for _, testCase := range cases {
		b.Run(fmt.Sprintf("From%dTo%d", testCase.from, testCase.to), func(b *testing.B) {
			field, err := NewGeoField(IndexTypeLocation, nil, FieldConfig{})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = field.Close() })
			for id := 1; id <= testCase.from; id++ {
				if err := field.Add(Entry{ImageID: ImageID(id), Value: GeoValue(paris)}); err != nil {
					b.Fatal(err)
				}
			}
			for id := 1000; id < 1000+testCase.to; id++ {
				if err := field.Add(Entry{ImageID: ImageID(id), Value: GeoValue(london)}); err != nil {
					b.Fatal(err)
				}
			}
			if err := field.Compact(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				if err := field.Move(Move{ImageID: 1, From: int64(paris), To: int64(london)}); err != nil {
					b.Fatal(err)
				}
				if err := field.Move(Move{ImageID: 1, From: int64(london), To: int64(paris)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkContainerFlip measures the full public path for a tiny<->bitmap
// promotion and demotion cycle, including publication and both folds.
func BenchmarkContainerFlip(b *testing.B) {
	paris, err := KeyFromLatLng(48.8566, 2.3522)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Enum", func(b *testing.B) {
		field, _, value := seededEnumField(b, 4)
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if err := field.Add(Entry{ImageID: 1000, Value: value}); err != nil {
				b.Fatal(err)
			}
			if err := field.Compact(); err != nil {
				b.Fatal(err)
			}
			if err := field.Remove(Entry{ImageID: 1000, Value: value}); err != nil {
				b.Fatal(err)
			}
			if err := field.Compact(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Geo", func(b *testing.B) {
		field, err := NewGeoField(IndexTypeLocation, nil, FieldConfig{})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = field.Close() })
		for id := 1; id <= 4; id++ {
			if err := field.Add(Entry{ImageID: ImageID(id), Value: GeoValue(paris)}); err != nil {
				b.Fatal(err)
			}
		}
		if err := field.Compact(); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for n := 0; n < b.N; n++ {
			if err := field.Add(Entry{ImageID: 1000, Value: GeoValue(paris)}); err != nil {
				b.Fatal(err)
			}
			if err := field.Compact(); err != nil {
				b.Fatal(err)
			}
			if err := field.Remove(Entry{ImageID: 1000, Value: GeoValue(paris)}); err != nil {
				b.Fatal(err)
			}
			if err := field.Compact(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
