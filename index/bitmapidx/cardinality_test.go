package bitmapidx

import (
	"math/rand"
	"reflect"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestNamespaceCardinalityTracksLiveValues(t *testing.T) {
	index := mustNamespace(t, enumNamespace(schema.NamespaceTag), NamespaceConfig{ActiveTarget: 4, HardDeltaLimit: 32, MaxDeltaValues: 8})
	a, b := EnumStringValue("a"), EnumStringValue("b")
	if got := index.Cardinality(); got != 0 {
		t.Fatal(got)
	}
	commit(t, index, func(bt *Batch) error {
		return bt.Add(
			Entry{ImageID: 1, Value: a},
			Entry{ImageID: 2, Value: a},
			Entry{ImageID: 2, Value: b},
		)
	})
	if got := index.Cardinality(); got != 2 {
		t.Fatalf("two values, including two on one image: %d", got)
	}
	commit(t, index, func(bt *Batch) error {
		return bt.Remove(Entry{ImageID: 2, Value: b})
	})
	if got := index.Cardinality(); got != 1 {
		t.Fatalf("removed last membership: %d", got)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := index.Cardinality(); got != 1 {
		t.Fatalf("after compact: %d", got)
	}
	commit(t, index, func(bt *Batch) error {
		return bt.Add(Entry{ImageID: 3, Value: b})
	})
	if got := index.Cardinality(); got != 2 {
		t.Fatalf("re-added value: %d", got)
	}
}

// The oracle scans effective postings, while Cardinality only adjusts keys
// touched by deltas. Repeated rotations exercise keys shared by Base, Frozen,
// and Active, plus values introduced and removed before a fold.
func TestNamespaceCardinalityAcrossRotations(t *testing.T) {
	index := mustNamespace(t, enumNamespace(schema.NamespaceTag), NamespaceConfig{ActiveTarget: 4, HardDeltaLimit: 32, MaxDeltaValues: 8})
	rng := rand.New(rand.NewSource(17))
	values := []Value{EnumStringValue("a"), EnumStringValue("b"), EnumStringValue("c"), EnumStringValue("d"), EnumStringValue("e")}
	for step := range 300 {
		entry := Entry{ImageID: schema.ImageID(rng.Intn(12) + 1), Value: values[rng.Intn(len(values))]}
		if rng.Intn(3) == 0 {
			commit(t, index, func(bt *Batch) error { return bt.Remove(entry) })
		} else {
			commit(t, index, func(bt *Batch) error { return bt.Add(entry) })
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
	namespaces := []NamespaceSpec{
		enumNamespace(schema.NamespaceTag),
		boolNamespace(schema.NamespaceFlag),
		continuousNamespace(schema.NamespaceRating),
		textNamespace(schema.NamespaceObject),
		enumNamespace(schema.NamespaceUserACL), // registered but empty
	}
	store, err := NewStore(dir, namespaces, NamespaceConfig{}, schema.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := store.Lookup(schema.NamespaceTag)
	flag, _ := store.Lookup(schema.NamespaceFlag)
	rating, _ := store.Lookup(schema.NamespaceRating)
	object, _ := store.Lookup(schema.NamespaceObject)
	for _, entry := range []Entry{
		{ImageID: 1, Value: EnumStringValue("a")},
		{ImageID: 1, Value: EnumStringValue("b")},
		{ImageID: 2, Value: EnumStringValue("a")},
	} {
		commit(t, tag, func(bt *Batch) error { return bt.Add(entry) })
	}
	commit(t, flag, func(bt *Batch) error {
		return bt.Add(Entry{ImageID: 1, Value: BoolValue(true)}, Entry{ImageID: 2, Value: BoolValue(false)})
	})
	commit(t, rating, func(bt *Batch) error {
		return bt.Move(Move{ImageID: 1, To: 7}, Move{ImageID: 2, To: 8})
	})
	commit(t, object, func(bt *Batch) error {
		return bt.ReplaceText(TextReplacement{ImageID: 1, After: []string{"red blue"}}, TextReplacement{ImageID: 2, After: []string{"blue green"}})
	})
	want := map[schema.Namespace]uint64{
		schema.NamespaceTag: 2, schema.NamespaceFlag: 2, schema.NamespaceRating: 2,
		schema.NamespaceObject: 3, schema.NamespaceUserACL: 0,
	}
	if got := store.Cardinalities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("before checkpoint: got %v, want %v", got, want)
	}
	if count, ok := store.Cardinality(schema.NamespaceObject); !ok || count != 3 {
		t.Fatalf("one type: %d, %v", count, ok)
	}
	if count, ok := store.Cardinality(schema.Namespace(500)); ok || count != 0 {
		t.Fatalf("unregistered type: %d, %v", count, ok)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, NamespaceConfig{}, schema.DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Cardinalities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after reopen: got %v, want %v", got, want)
	}
	object, _ = reopened.Lookup(schema.NamespaceObject)
	commit(t, object, func(bt *Batch) error {
		return bt.ReplaceText(TextReplacement{ImageID: 2, Before: []string{"blue green"}, After: []string{"red blue"}})
	})
	want[schema.NamespaceObject] = 2
	if got := reopened.Cardinalities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after replacement: got %v, want %v", got, want)
	}
}
