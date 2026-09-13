package bitmapidx

import (
	"errors"
	"slices"
	"sync"
	"testing"
)

func TestCompiledQueryParity(t *testing.T) {
	index, names := benchmarkEnumField(t, 4, 20)
	defer func() { _ = index.Close() }()

	compiled, err := index.Compile(names...)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	direct, err := index.MatchValues(names...)
	if err != nil {
		t.Fatalf("MatchValues: %v", err)
	}
	got, err := compiled.Match()
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !slices.Equal(got.AppendTo(nil), direct.AppendTo(nil)) {
		t.Fatal("compiled Match differs from MatchValues")
	}
	directCount, err := index.CountValues(names...)
	if err != nil {
		t.Fatalf("CountValues: %v", err)
	}
	gotCount, err := compiled.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if gotCount != directCount {
		t.Fatalf("compiled Count = %d, want %d", gotCount, directCount)
	}
	candidates := []ImageID{1, 2, 3, 4, 5, 999_999}
	directFiltered, err := index.FilterValues(nil, candidates, names...)
	if err != nil {
		t.Fatalf("FilterValues: %v", err)
	}
	gotFiltered, err := compiled.Filter(nil, candidates)
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if !slices.Equal(gotFiltered, directFiltered) {
		t.Fatal("compiled Filter differs from FilterValues")
	}
}

func TestCompiledQuerySeesLaterWrites(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("a")}); err != nil {
		t.Fatal(err)
	}
	compiled, err := index.Compile(StringValue("b"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	empty, err := compiled.Match()
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !empty.IsEmpty() {
		t.Fatal("compiled query matched before the value existed")
	}
	// A value added after compiling is visible without recompiling.
	if err := index.Add(Entry{ImageID: 2, Value: StringValue("b")}); err != nil {
		t.Fatal(err)
	}
	got, err := compiled.Match()
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !slices.Equal(got.AppendTo(nil), []ImageID{2}) {
		t.Fatalf("compiled Match = %v, want [2]", got.AppendTo(nil))
	}

	// Multi-value conjunctions see later writes too: image 3 holds both.
	if err := index.Add(
		Entry{ImageID: 3, Value: StringValue("a")},
		Entry{ImageID: 3, Value: StringValue("b")},
	); err != nil {
		t.Fatal(err)
	}
	both, err := index.Compile(StringValue("a"), StringValue("b"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got, err = both.Match()
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !slices.Equal(got.AppendTo(nil), []ImageID{3}) {
		t.Fatalf("compiled Match = %v, want [3]", got.AppendTo(nil))
	}
	count, err := both.Count()
	if err != nil || count != 1 {
		t.Fatalf("compiled Count = %d, %v", count, err)
	}

	// A value vacuumed away empties the conjunction.
	if err := index.Remove(
		Entry{ImageID: 2, Value: StringValue("b")},
		Entry{ImageID: 3, Value: StringValue("b")},
	); err != nil {
		t.Fatal(err)
	}
	if err := index.Vacuum(); err != nil {
		t.Fatal(err)
	}
	got, err = both.Match()
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !got.IsEmpty() {
		t.Fatalf("compiled Match after vacuum = %v, want empty", got.AppendTo(nil))
	}
}

func TestCompiledQueryValidation(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})

	empty, err := index.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got, err := empty.Match()
	if err != nil || !got.IsEmpty() {
		t.Fatalf("empty query matched %v, %v", got.AppendTo(nil), err)
	}

	if _, err := index.Compile(IntValue(7)); !errors.Is(err, ErrValueKind) {
		t.Fatalf("wrong-kind Compile error = %v", err)
	}
	if _, err := index.CompileText("anything"); !errors.Is(err, ErrFieldMismatch) {
		t.Fatalf("CompileText on enum error = %v", err)
	}

	text := mustField(t, textField(IndexTypeTag), FieldConfig{})
	if err := text.ReplaceText(TextReplacement{ImageID: 1, After: []string{"alpha beta"}}); err != nil {
		t.Fatal(err)
	}
	compiled, err := text.CompileText("alpha beta")
	if err != nil {
		t.Fatalf("CompileText: %v", err)
	}
	direct, err := text.MatchText("alpha beta")
	if err != nil {
		t.Fatalf("MatchText: %v", err)
	}
	got, err = compiled.Match()
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !slices.Equal(got.AppendTo(nil), direct.AppendTo(nil)) {
		t.Fatal("compiled text differs from MatchText")
	}

	closed, err := NewFieldIndex(enumField(IndexTypeTag), FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	closedQuery, err := closed.Compile(StringValue("a"))
	if err != nil {
		t.Fatalf("Compile on closed index error = %v", err)
	}
	if got, err := closedQuery.Match(); err != nil || !got.IsEmpty() {
		t.Fatalf("closed Compile match = %v, %v; want empty, nil", got.AppendTo(nil), err)
	}
	stale, err := index.Compile(StringValue("a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := stale.Match(); err != nil || !got.IsEmpty() {
		t.Fatalf("Match after close = %v, %v", got.AppendTo(nil), err)
	}
}

func TestZeroQueryMatchesNothing(t *testing.T) {
	var q Query
	if got, err := q.Match(); err != nil || !got.IsEmpty() {
		t.Fatalf("zero Query Match = %v, %v; want empty, nil", got.AppendTo(nil), err)
	}
	if count, err := q.Count(); err != nil || count != 0 {
		t.Fatalf("zero Query Count = %d, %v; want 0, nil", count, err)
	}
	if got, err := q.Filter([]ImageID{9}, []ImageID{1}); err != nil || !slices.Equal(got, []ImageID{9}) {
		t.Fatalf("zero Query Filter = %v, %v; want [9], nil", got, err)
	}
}

func TestCompiledQueryConcurrent(t *testing.T) {
	index, names := benchmarkEnumField(t, 4, 1_000)
	defer func() { _ = index.Close() }()
	compiled, err := index.Compile(names...)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for n := 0; n < 50; n++ {
				if _, err := compiled.Match(); err != nil {
					t.Error(err)
					return
				}
				if _, err := compiled.Count(); err != nil {
					t.Error(err)
					return
				}
				if _, err := compiled.Filter(nil, []ImageID{1, 2, 3}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
}
