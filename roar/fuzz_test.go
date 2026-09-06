package roar

import (
	"encoding/binary"
	"slices"
	"testing"
)

// FuzzOpen verifies that arbitrary untrusted bytes never panic validation
// or produce out-of-bounds unsafe access.
func FuzzOpen(f *testing.F) {
	seeds := [][]byte{}
	for _, ids := range [][]uint64{
		nil,
		{1 << 48},
		{1<<48 + 1, 1<<48 + 2, 1<<48 + 3, 1<<48 + 4},
		{1<<48 + 1, 2<<48 + 9, 3<<48 + 65535},
		denseSortedIDs(rngForTest(), 2),
	} {
		bitmap, err := FromSorted(ids)
		if err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, bitmap.Bytes())
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		view, err := Open(data)
		if err != nil {
			return
		}
		_ = view.Cardinality()
		_ = view.KeyCount()
		for iterator := view.Iterator(); iterator.Next(); {
			_ = iterator.Value()
		}
	})
}

// FuzzFromSorted verifies that building from arbitrary ID material is panic
// free and that a valid build round-trips through Open with a matching
// stored cardinality.
func FuzzFromSorted(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{5, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		ids := sortedUnique(bytesToIDs(data))
		bitmap, err := FromSorted(ids)
		if err != nil {
			t.Fatalf("FromSorted(sorted unique non-zero) = %v", err)
		}
		if bitmap.Cardinality() != uint64(len(ids)) {
			t.Fatalf("Cardinality = %d, want %d", bitmap.Cardinality(), len(ids))
		}
		reopened, err := Open(bitmap.Bytes())
		if err != nil {
			t.Fatalf("Open round trip = %v", err)
		}
		if reopened.Cardinality() != uint64(len(ids)) {
			t.Fatalf("round-trip cardinality = %d, want %d", reopened.Cardinality(), len(ids))
		}
	})
}

// FuzzDeltaBuilder verifies that the lazy mutation path matches the
// reference (base ∪ add) ∖ remove for arbitrary inputs.
func FuzzDeltaBuilder(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		third := len(data) / 3
		base := sortedUnique(bytesToIDs(data[:third]))
		adds := sortedUnique(bytesToIDs(data[third : 2*third]))
		removes := sortedUnique(bytesToIDs(data[2*third:]))

		baseBm, err := FromSorted(base)
		if err != nil {
			t.Fatal(err)
		}
		addBm, err := FromSorted(adds)
		if err != nil {
			t.Fatal(err)
		}
		removeBm, err := FromSorted(removes)
		if err != nil {
			t.Fatal(err)
		}

		builder := NewDeltaBuilderFrom(baseBm)
		builder.AddMany(adds)
		for _, id := range removes {
			builder.Remove(id)
		}
		got := builder.Freeze().AppendTo(nil)

		want := AndNot(Or(baseBm, addBm), removeBm).AppendTo(nil)
		if !slices.Equal(got, want) {
			t.Fatalf("mutable builder mismatch: got %v, want %v", got, want)
		}
	})
}

func bytesToIDs(data []byte) []uint64 {
	ids := make([]uint64, 0, len(data)/8)
	for len(data) >= 8 {
		ids = append(ids, binary.LittleEndian.Uint64(data[:8]))
		data = data[8:]
	}
	return ids
}

func sortedUnique(ids []uint64) []uint64 {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) != 0 && ids[0] == InvalidImageID {
		ids = ids[1:]
	}
	return ids
}
