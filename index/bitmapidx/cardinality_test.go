package bitmapidx

import (
	"math/rand"
	"reflect"
	"testing"
)

func TestFieldCardinalityTracksLiveValues(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{ActiveTarget: 4, HardDeltaLimit: 32, MaxDeltaValues: 8})
	a, b := StringValue("a"), StringValue("b")
	if got := index.Cardinality(); got != 0 {
		t.Fatal(got)
	}
	if err := index.Add(
		Entry{ImageID: 1, Value: a},
		Entry{ImageID: 2, Value: a},
		Entry{ImageID: 2, Value: b},
	); err != nil {
		t.Fatal(err)
	}
	if got := index.Cardinality(); got != 2 {
		t.Fatalf("two values, including two on one image: %d", got)
	}
	if err := index.Remove(Entry{ImageID: 2, Value: b}); err != nil {
		t.Fatal(err)
	}
	if got := index.Cardinality(); got != 1 {
		t.Fatalf("removed last membership: %d", got)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := index.Cardinality(); got != 1 {
		t.Fatalf("after compact: %d", got)
	}
	if err := index.Add(Entry{ImageID: 3, Value: b}); err != nil {
		t.Fatal(err)
	}
	if got := index.Cardinality(); got != 2 {
		t.Fatalf("re-added value: %d", got)
	}
}

// The oracle scans effective postings, while Cardinality only adjusts keys
// touched by deltas. Repeated rotations exercise keys shared by Base, Frozen,
// and Active, plus values introduced and removed before a fold.
func TestFieldCardinalityAcrossRotations(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{ActiveTarget: 4, HardDeltaLimit: 32, MaxDeltaValues: 8})
	rng := rand.New(rand.NewSource(17))
	values := []Value{StringValue("a"), StringValue("b"), StringValue("c"), StringValue("d"), StringValue("e")}
	for step := range 300 {
		entry := Entry{ImageID: ImageID(rng.Intn(12) + 1), Value: values[rng.Intn(len(values))]}
		var err error
		if rng.Intn(3) == 0 {
			err = index.Remove(entry)
		} else {
			err = index.Add(entry)
		}
		if err != nil {
			t.Fatal(err)
		}
		if step%37 == 0 {
			if err := index.Compact(); err != nil {
				t.Fatal(err)
			}
		}
		view := index.view.Load()
		var want uint64
		for _, key := range view.allKeys() {
			if posting, ok := view.effective(key); ok && !posting.isEmpty() {
				want++
			}
		}
		if got := index.Cardinality(); got != want {
			t.Fatalf("step %d: Cardinality=%d, effective=%d", step, got, want)
		}
	}
}

func TestIndexCardinalitiesByTypeAndReopen(t *testing.T) {
	dir := t.TempDir()
	fields := []Field{
		enumField(IndexTypeTag),
		boolField(IndexTypeFlag),
		continuousField(IndexTypeRating),
		textField(IndexTypeObject),
		geoFieldFor(IndexTypeLocation),
		enumField(IndexTypeUserACL), // registered but empty
	}
	store, err := NewIndex(dir, fields, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := store.Lookup(IndexTypeTag)
	flag, _ := store.Lookup(IndexTypeFlag)
	rating, _ := store.Lookup(IndexTypeRating)
	object, _ := store.Lookup(IndexTypeObject)
	location, _ := store.Lookup(IndexTypeLocation)
	for _, entry := range []Entry{
		{ImageID: 1, Value: StringValue("a")},
		{ImageID: 1, Value: StringValue("b")},
		{ImageID: 2, Value: StringValue("a")},
	} {
		if err := tag.Add(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := flag.Add(Entry{ImageID: 1, Value: BoolValue(true)}, Entry{ImageID: 2, Value: BoolValue(false)}); err != nil {
		t.Fatal(err)
	}
	if err := rating.Move(Move{ImageID: 1, To: 7, HasTo: true}, Move{ImageID: 2, To: 8, HasTo: true}); err != nil {
		t.Fatal(err)
	}
	if err := object.ReplaceText(TextReplacement{ImageID: 1, After: []string{"red blue"}}, TextReplacement{ImageID: 2, After: []string{"blue green"}}); err != nil {
		t.Fatal(err)
	}
	cell, err := KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	if err := location.Move(Move{ImageID: 1, To: int64(cell), HasTo: true}); err != nil {
		t.Fatal(err)
	}
	want := map[IndexType]uint64{
		IndexTypeTag: 2, IndexTypeFlag: 2, IndexTypeRating: 2,
		IndexTypeObject: 3, IndexTypeLocation: 1, IndexTypeUserACL: 0,
	}
	if got := store.Cardinalities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("before checkpoint: got %v, want %v", got, want)
	}
	if count, ok := store.Cardinality(IndexTypeObject); !ok || count != 3 {
		t.Fatalf("one type: %d, %v", count, ok)
	}
	if count, ok := store.Cardinality(IndexType(500)); ok || count != 0 {
		t.Fatalf("unregistered type: %d, %v", count, ok)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenIndex(dir, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Cardinalities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after reopen: got %v, want %v", got, want)
	}
	object, _ = reopened.Lookup(IndexTypeObject)
	if err := object.ReplaceText(TextReplacement{ImageID: 2, Before: []string{"blue green"}, After: []string{"red blue"}}); err != nil {
		t.Fatal(err)
	}
	want[IndexTypeObject] = 2
	if got := reopened.Cardinalities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after replacement: got %v, want %v", got, want)
	}
}
