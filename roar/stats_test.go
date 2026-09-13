package roar

import (
	"testing"
)

func TestStats(t *testing.T) {
	var empty Bitmap
	if got := empty.Stats(); got != (Stats{}) {
		t.Fatalf("empty Stats = %+v, want zero", got)
	}
	if v := empty.Version(); v != 0 {
		t.Fatalf("empty Version = %d, want 0", v)
	}
	if f := empty.Flags(); f != 0 {
		t.Fatalf("empty Flags = %d, want 0", f)
	}

	sparse, err := FromSorted([]uint64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	got := sparse.Stats()
	if got.Containers != 1 || got.Arrays != 1 || got.Bitmaps != 0 {
		t.Fatalf("sparse Stats containers = %+v", got)
	}
	if got.Cardinality != 3 || got.Bytes != sparse.LenInBytes() {
		t.Fatalf("sparse Stats totals = %+v", got)
	}
	if v := sparse.Version(); v != Version {
		t.Fatalf("Version = %d, want %d", v, Version)
	}
	if f := sparse.Flags(); f&(FlagCompact|FlagStoredCardinality) == 0 {
		t.Fatalf("Flags = %d, missing compact/cardinality bits", f)
	}

	dense := FromUnsorted(denseSortedIDs(rngForTest(), 2))
	mixed := Or(sparse, dense)
	stats := mixed.Stats()
	if stats.Containers != mixed.KeyCount() {
		t.Fatalf("Containers = %d, want %d", stats.Containers, mixed.KeyCount())
	}
	if stats.Arrays+stats.Bitmaps != stats.Containers {
		t.Fatalf("Arrays+Bitmaps = %d, want %d", stats.Arrays+stats.Bitmaps, stats.Containers)
	}
	if stats.Cardinality != mixed.Cardinality() {
		t.Fatalf("Cardinality = %d, want %d", stats.Cardinality, mixed.Cardinality())
	}
	if stats.Bytes != mixed.LenInBytes() {
		t.Fatalf("Bytes = %d, want %d", stats.Bytes, mixed.LenInBytes())
	}
	// Cross-check the type walk against a direct header scan.
	var arrays int
	for _, entry := range mixed.keyEntries() {
		if ContainerType(containerHeaderAt(mixed.data[entry.Offset:]).Type) == ContainerArray {
			arrays++
		}
	}
	if stats.Arrays != arrays {
		t.Fatalf("Arrays = %d, want %d", stats.Arrays, arrays)
	}
}
