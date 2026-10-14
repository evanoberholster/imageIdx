package bitmapidx

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

// TestCountValuesMatchesMaterialized checks that the single-key count fast
// path (base + delta additions - removals) equals the materialized posting
// cardinality across rotations, folds, and compaction.
func TestCountValuesMatchesMaterialized(t *testing.T) {
	index := mustNamespace(t, enumNamespace(schema.NamespaceTag), NamespaceConfig{ActiveTarget: 4, HardDeltaLimit: 32, MaxDeltaValues: 8})
	value := EnumStringValue("v")
	other := EnumStringValue("other")
	for step := 0; step < 600; step++ {
		id := schema.ImageID((step*7)%64 + 1)
		switch {
		case step%3 == 0:
			commit(t, index, func(b *Batch) error {
				return b.Remove(Entry{ImageID: id, Value: value})
			})
		case step%5 == 0:
			commit(t, index, func(b *Batch) error {
				return b.Add(Entry{ImageID: id, Value: other})
			})
		default:
			commit(t, index, func(b *Batch) error {
				return b.Add(Entry{ImageID: id, Value: value})
			})
		}
		count, err := index.CountValues(value)
		if err != nil {
			t.Fatal(err)
		}
		match, err := index.MatchValues(value)
		if err != nil {
			t.Fatal(err)
		}
		if got := match.Cardinality(); count != got {
			t.Fatalf("step %d: CountValues=%d, materialized=%d", step, count, got)
		}
	}
	for n := 0; n < 40; n++ {
		id := schema.ImageID(n%32 + 1)
		if n%2 == 0 {
			commit(t, index, func(b *Batch) error {
				return b.Add(Entry{ImageID: id, Value: value})
			})
		} else {
			commit(t, index, func(b *Batch) error {
				return b.Remove(Entry{ImageID: id, Value: value})
			})
		}
		if err := index.Compact(); err != nil {
			t.Fatal(err)
		}
		count, _ := index.CountValues(value)
		match, _ := index.MatchValues(value)
		if count != match.Cardinality() {
			t.Fatalf("compact step %d: CountValues=%d, materialized=%d", n, count, match.Cardinality())
		}
	}
}

// TestQueryResultsDetachedAcrossRetirement holds query results while the
// segment generations they were built from are vacuumed away. A result that
// aliases a retired mapping instead of being detached would be read after
// unmap here.
func TestQueryResultsDetachedAcrossRetirement(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, []NamespaceSpec{enumNamespace(schema.NamespaceTag)}, NamespaceConfig{}, schema.DurabilityOS)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	defer store.Close()
	tag, _ := store.Lookup(schema.NamespaceTag)

	for id := 1; id <= 128; id++ {
		commit(t, tag, func(b *Batch) error {
			return b.Add(Entry{ImageID: schema.ImageID(id), Value: EnumStringValue("shared")})
		})
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Base-only results alias the mapped segment and must be detached.
	baseMatch, err := tag.MatchValues(EnumStringValue("shared"))
	if err != nil {
		t.Fatal(err)
	}

	// Mutate so the next results take the corrected, heap-owned path.
	commit(t, tag, func(b *Batch) error {
		return b.Add(Entry{ImageID: 500, Value: EnumStringValue("shared")})
	})
	deltaMatch, err := tag.MatchValues(EnumStringValue("shared"))
	if err != nil {
		t.Fatal(err)
	}

	// Retire every mapping the base results may have aliased.
	for n := 0; n < 3; n++ {
		if err := store.Vacuum(); err != nil {
			t.Fatal(err)
		}
	}

	if got := baseMatch.AppendTo(nil); len(got) != 128 {
		t.Fatalf("base enum result = %d ids, want 128", len(got))
	}
	if got := deltaMatch.AppendTo(nil); len(got) != 129 {
		t.Fatalf("delta enum result = %d ids, want 129", len(got))
	}
}
