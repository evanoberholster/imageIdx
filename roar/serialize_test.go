package roar

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
)

func TestLenInBytes(t *testing.T) {
	empty, err := FromSorted(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.LenInBytes(); got != HeaderSize {
		t.Fatalf("empty LenInBytes = %d, want %d", got, HeaderSize)
	}
	data, err := empty.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if empty.LenInBytes() != len(data) {
		t.Fatalf("empty LenInBytes = %d, MarshalBinary = %d", empty.LenInBytes(), len(data))
	}

	bitmap, err := FromSorted([]uint64{1, 2, 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bitmap.LenInBytes(), len(bitmap.Bytes()); got != want {
		t.Fatalf("LenInBytes = %d, want %d", got, want)
	}
}

func TestStoredCardinality(t *testing.T) {
	ids := denseSortedIDs(rngForTest(), 3)
	bitmap, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}
	data := bitmap.Bytes()
	if flags := binary.LittleEndian.Uint16(data[6:8]); flags&FlagStoredCardinality == 0 {
		t.Fatal("constructor did not set FlagStoredCardinality")
	}
	if stored := binary.LittleEndian.Uint64(data[16:24]); stored != uint64(len(ids)) {
		t.Fatalf("stored cardinality = %d, want %d", stored, len(ids))
	}
	if got := bitmap.Cardinality(); got != uint64(len(ids)) {
		t.Fatalf("Cardinality = %d, want %d", got, len(ids))
	}
}

func TestStoredCardinalityValidated(t *testing.T) {
	bitmap, err := FromSorted([]uint64{1, 2, 3, 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := slices.Clone(bitmap.Bytes())
	binary.LittleEndian.PutUint64(corrupt[16:24], 999)
	if err := Validate(corrupt); err == nil {
		t.Fatal("Validate accepted a mismatched stored cardinality")
	}
}

func TestCardinalityFallbackWithoutFlag(t *testing.T) {
	bitmap, err := FromSorted([]uint64{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	// Clear the stored-cardinality flag to exercise the summing fallback.
	data := slices.Clone(bitmap.Bytes())
	binary.LittleEndian.PutUint16(data[6:8], FlagCompact)
	view, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Cardinality(); got != 5 {
		t.Fatalf("fallback Cardinality = %d, want 5", got)
	}
}

func TestString(t *testing.T) {
	var empty Bitmap
	if got := empty.String(); got != "Bitmap{containers=0, cardinality=0, bytes=0, arrays=0, bitmaps=0}" {
		t.Fatalf("empty String = %q", got)
	}

	bitmap, err := FromSorted([]uint64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("Bitmap{containers=1, cardinality=3, bytes=%d, arrays=1, bitmaps=0}", bitmap.LenInBytes())
	if got := bitmap.String(); got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
}
