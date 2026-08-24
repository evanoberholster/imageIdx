package roar

import "testing"

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
		data, err := FromSorted(ids)
		if err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, data)
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
