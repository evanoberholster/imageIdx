package bitmapidx

import (
	"fmt"
	"testing"
)

func benchmarkEnumField(b *testing.B, values int, perValue int) (*FieldIndex, []Value) {
	b.Helper()
	index, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
	if err != nil {
		b.Fatalf("NewFieldIndex: %v", err)
	}
	b.Cleanup(func() { _ = index.Close() })
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
		b.Fatalf("Add: %v", err)
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
