package radixhash

import (
	"math/rand"
	"sort"
	"testing"
)

// walkFixture builds a promoted index and returns the expected entries sorted
// by (hash, ImageID), matching the walker's radix/hash order.
type walkPair struct {
	hash Hash
	id   ImageID
}

func walkFixture(t *testing.T, n int) (*Index, []walkPair) {
	t.Helper()
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 2048})
	rng := rand.New(rand.NewSource(7))
	model := make(map[Hash][]ImageID)
	var batch []Entry
	for id := ImageID(1); id <= ImageID(n); id++ {
		h := hashWithPrefix([]byte{byte(rng.Intn(64)), byte(rng.Intn(8))}, uint64(id))
		model[h] = append(model[h], id)
		batch = append(batch, Entry{Hash: h, ImageID: id})
		if len(batch) == 256 {
			if err := index.Apply(batch); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			batch = batch[:0]
		}
	}
	if err := index.Apply(batch); err != nil {
		t.Fatalf("Apply tail: %v", err)
	}
	expected := make([]walkPair, 0, n)
	for h, ids := range model {
		for _, id := range ids {
			expected = append(expected, walkPair{hash: h, id: id})
		}
	}
	sort.Slice(expected, func(a, b int) bool {
		if c := compareHashOrder(expected[a].hash, expected[b].hash); c != 0 {
			return c < 0
		}
		return expected[a].id < expected[b].id
	})
	return index, expected
}

func compareHashOrder(a, b Hash) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// TestEntryWalkerStreamsSortedEntries verifies the bounded walker yields every
// entry exactly once, in (hash, ImageID) order, with a buffer far smaller than
// the index.
func TestEntryWalkerStreamsSortedEntries(t *testing.T) {
	index, expected := walkFixture(t, 5000)
	defer index.Close()

	walker := newEntryWalker(index.view.Load())
	buf := make([]Entry, 3)
	var got []Entry
	for {
		n, more, err := walker.Next(buf)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, buf[:n]...)
		if !more {
			break
		}
	}
	if len(got) != len(expected) {
		t.Fatalf("walked %d entries, want %d", len(got), len(expected))
	}
	for i := range got {
		if got[i].Hash != expected[i].hash || got[i].ImageID != expected[i].id {
			t.Fatalf("entry %d = (%x,%d), want (%x,%d)", i, got[i].Hash[:4], got[i].ImageID, expected[i].hash[:4], expected[i].id)
		}
	}
}

// TestCompactStreamingBatchSize verifies compaction preserves the index for
// batch sizes from one entry up, forcing many bounded walk/apply rounds.
func TestCompactStreamingBatchSize(t *testing.T) {
	original := compactBatchEntries
	defer func() { compactBatchEntries = original }()

	for _, size := range []int{1, 7, 4096} {
		compactBatchEntries = size
		index, expected := walkFixture(t, 3000)
		if err := index.Compact(); err != nil {
			t.Fatalf("Compact(batch=%d): %v", size, err)
		}
		if got := index.Metadata().Entries; got != uint64(len(expected)) {
			t.Fatalf("entries after Compact(batch=%d) = %d, want %d", size, got, len(expected))
		}
		for _, pair := range expected {
			if got := index.Lookup(nil, pair.hash); !containsID(got, pair.id) {
				t.Fatalf("Lookup(%x) missing id %d after Compact(batch=%d)", pair.hash[:4], pair.id, size)
			}
		}
		if err := index.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func containsID(ids []ImageID, want ImageID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
