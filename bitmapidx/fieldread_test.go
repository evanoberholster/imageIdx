package bitmapidx

import "testing"

// TestCountValuesMatchesMaterialized checks that the single-key count fast
// path (base + delta additions - removals) equals the materialized posting
// cardinality across rotations, folds, and compaction.
func TestCountValuesMatchesMaterialized(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{ActiveTarget: 4, HardDeltaLimit: 32, MaxDeltaValues: 8})
	value := StringValue("v")
	other := StringValue("other")
	for step := 0; step < 600; step++ {
		id := ImageID((step*7)%64 + 1)
		switch {
		case step%3 == 0:
			if err := index.Remove(Entry{ImageID: id, Value: value}); err != nil {
				t.Fatal(err)
			}
		case step%5 == 0:
			if err := index.Add(Entry{ImageID: id, Value: other}); err != nil {
				t.Fatal(err)
			}
		default:
			if err := index.Add(Entry{ImageID: id, Value: value}); err != nil {
				t.Fatal(err)
			}
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
		id := ImageID(n%32 + 1)
		if n%2 == 0 {
			if err := index.Add(Entry{ImageID: id, Value: value}); err != nil {
				t.Fatal(err)
			}
		} else if err := index.Remove(Entry{ImageID: id, Value: value}); err != nil {
			t.Fatal(err)
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
	store, err := NewPersistentFields(dir, []Field{enumField(IndexTypeTag), geoFieldFor(IndexTypeLocation)}, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatalf("NewPersistentFields: %v", err)
	}
	defer store.Close()
	tag, _ := store.Lookup(IndexTypeTag)
	loc, _ := store.Lookup(IndexTypeLocation)

	key, err := KeyFromLatLng(37.7749, -122.4194)
	if err != nil {
		t.Fatalf("KeyFromLatLng: %v", err)
	}
	for id := 1; id <= 128; id++ {
		if err := tag.Add(Entry{ImageID: ImageID(id), Value: StringValue("shared")}); err != nil {
			t.Fatal(err)
		}
		if err := loc.Move(Move{ImageID: ImageID(id), To: int64(key)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Base-only results alias the mapped segment and must be detached.
	baseMatch, err := tag.MatchValues(StringValue("shared"))
	if err != nil {
		t.Fatal(err)
	}
	baseGeo, err := loc.GeoRanges([]GeoRange{{Start: key, End: key}})
	if err != nil {
		t.Fatal(err)
	}

	// Mutate so the next results take the corrected, heap-owned path.
	if err := tag.Add(Entry{ImageID: 500, Value: StringValue("shared")}); err != nil {
		t.Fatal(err)
	}
	if err := loc.Move(Move{ImageID: 500, To: int64(key)}); err != nil {
		t.Fatal(err)
	}
	deltaMatch, err := tag.MatchValues(StringValue("shared"))
	if err != nil {
		t.Fatal(err)
	}
	deltaGeo, err := loc.GeoRanges([]GeoRange{{Start: key, End: key}})
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
	if got := baseGeo.AppendTo(nil); len(got) != 128 {
		t.Fatalf("base geo result = %d ids, want 128", len(got))
	}
	if got := deltaGeo.AppendTo(nil); len(got) != 129 {
		t.Fatalf("delta geo result = %d ids, want 129", len(got))
	}
}
