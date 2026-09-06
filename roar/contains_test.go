package roar

import "testing"

func TestContains(t *testing.T) {
	ids := []uint64{
		1<<48 + 1, 1<<48 + 2, 1<<48 + 3, 1<<48 + 4, // tiny array
		2<<48 + 10, 2<<48 + 50000, // sparse key
	}
	dense := make([]uint64, 0, 4096)
	for value := 0; value < 4096; value++ {
		dense = append(dense, 3<<48|uint64(value))
	}
	ids = append(ids, dense...)
	ids = append(ids, 4<<48+65535) // maximum low value

	view, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range ids {
		if !view.Contains(id) {
			t.Fatalf("Contains(%d) = false", id)
		}
	}
	absent := []uint64{0, 1, 1<<48 + 5, 5 << 48, 2<<48 + 11, 3<<48 + 4096, ^uint64(0)}
	for _, id := range absent {
		if view.Contains(id) {
			t.Fatalf("Contains(%d) = true", id)
		}
	}
}

func TestIteratorRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name string
		ids  []uint64
	}{
		{"empty", nil},
		{"one", []uint64{1<<48 + 7}},
		{"tiny", []uint64{1<<48 + 1, 1<<48 + 2, 1<<48 + 3, 1<<48 + 4}},
		{"mixed keys", []uint64{1<<48 + 1, 2<<48 + 9, 2<<48 + 65535, 5 << 48}},
	} {
		t.Run(test.name, func(t *testing.T) {
			view, err := FromSorted(test.ids)
			if err != nil {
				t.Fatal(err)
			}
			var got []uint64
			for iterator := view.Iterator(); iterator.Next(); {
				got = append(got, iterator.Value())
			}
			if len(got) != len(test.ids) {
				t.Fatalf("iterated %d IDs, want %d", len(got), len(test.ids))
			}
			for index := range got {
				if got[index] != test.ids[index] {
					t.Fatalf("value %d = %d, want %d", index, got[index], test.ids[index])
				}
			}
		})
	}

	t.Run("dense containers", func(t *testing.T) {
		ids := denseSortedIDs(rngForTest(), 3)
		view, err := FromSorted(ids)
		if err != nil {
			t.Fatal(err)
		}
		iterator := view.Iterator()
		for index := 0; index < len(ids); index++ {
			if !iterator.Next() {
				t.Fatalf("iterator exhausted at %d of %d", index, len(ids))
			}
			if iterator.Value() != ids[index] {
				t.Fatalf("value %d = %d, want %d", index, iterator.Value(), ids[index])
			}
		}
		if iterator.Next() {
			t.Fatal("iterator continued past the end")
		}
	})

	t.Run("zero iterator", func(t *testing.T) {
		var iterator Iterator
		if iterator.Next() {
			t.Fatal("zero iterator produced a value")
		}
	})
}
