package bitmapidx

import (
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
)

func TestFilterValuesPreservesCandidateOrderWithDeltas(t *testing.T) {
	index := mustNamespace(t, enumNamespace(common.NamespaceTag), NamespaceConfig{})
	defer index.Close()
	value := StringValue("review-filter")
	commit(t, index, func(batch *Batch) error {
		return batch.Add(Entry{ImageID: 1, Value: value}, Entry{ImageID: 3, Value: value})
	})
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	commit(t, index, func(batch *Batch) error {
		return batch.Add(Entry{ImageID: 4, Value: value})
	})
	candidates := []ImageID{3, 2, 4, 1, 3}
	got, err := index.FilterValues(candidates[:0], candidates, value)
	if err != nil || !slices.Equal(got, []ImageID{3, 4, 1, 3}) {
		t.Fatalf("FilterValues = %v, %v", got, err)
	}
}

func TestEffectiveTableMergesLayeredChanges(t *testing.T) {
	key := func(n int64) NamespaceKey { return continuousKey(common.NamespaceRating, n) }
	base := postingTable{
		keys: []NamespaceKey{key(1), key(3), key(5)},
		postings: []posting{
			makePosting(roar.FromUnsorted([]uint64{1})),
			makePosting(roar.FromUnsorted([]uint64{3})),
			makePosting(roar.FromUnsorted([]uint64{5})),
		},
	}
	frozen := &namespaceDelta{
		keys: []NamespaceKey{key(1), key(2), key(3)},
		values: map[NamespaceKey]*bitmapDelta{
			key(1): newBitmapDelta(nil, []ImageID{1}),
			key(2): newBitmapDelta([]ImageID{2}, nil),
			key(3): newBitmapDelta([]ImageID{30}, nil),
		},
	}
	active := &namespaceDelta{
		keys: []NamespaceKey{key(1), key(2), key(3), key(4)},
		values: map[NamespaceKey]*bitmapDelta{
			key(1): newBitmapDelta([]ImageID{1}, nil),
			key(2): newBitmapDelta(nil, []ImageID{2}),
			key(3): newBitmapDelta(nil, []ImageID{3}),
			key(4): newBitmapDelta([]ImageID{4}, nil),
		},
	}
	view := &namespaceView{base: base}
	got := view.effectiveTable(frozen, active)
	wantKeys := []NamespaceKey{key(1), key(3), key(4), key(5)}
	if !slices.Equal(got.keys, wantKeys) {
		t.Fatalf("keys = %v, want %v", got.keys, wantKeys)
	}
	for n, wantID := range []ImageID{1, 30, 4, 5} {
		if ids := got.postings[n].appendTo(nil); !slices.Equal(ids, []ImageID{wantID}) {
			t.Fatalf("posting %d = %v, want %d", n, ids, wantID)
		}
	}
	if ids := base.postings[0].appendTo(nil); !slices.Equal(ids, []ImageID{1}) {
		t.Fatalf("source base mutated: %v", ids)
	}
}

func TestContinuousKeysMergeSortedLayers(t *testing.T) {
	key := func(n int64) NamespaceKey { return continuousKey(common.NamespaceRating, n) }
	view := &namespaceView{
		base:   postingTable{keys: []NamespaceKey{key(1), key(3), key(5)}},
		frozen: &namespaceDelta{keys: []NamespaceKey{key(2), key(3)}},
		active: &namespaceDelta{keys: []NamespaceKey{key(3), key(4)}},
	}
	want := []NamespaceKey{key(1), key(2), key(3), key(4)}
	if got := view.continuousKeys(Between(1, 5)); !slices.Equal(got, want) {
		t.Fatalf("continuous keys = %v, want %v", got, want)
	}
}

func TestDictionaryMergedPrefersActive(t *testing.T) {
	key := continuousKey(common.NamespaceRating, 1)
	view := &dictionaryView{
		base:   []dictEntry{{key: key, chunk: 0, length: 4}},
		active: []dictEntry{{key: key, chunk: 1, length: 6}},
		chunks: [][]byte{[]byte("base"), []byte("active")},
	}
	entries := view.merged()
	if len(entries) != 1 || string(view.bytes(entries[0])) != "active" {
		t.Fatalf("merged entries = %v", entries)
	}
}

func BenchmarkEffectiveTableWideDelta(b *testing.B) {
	const baseCount, deltaCount = 1024, 256
	key := func(n int64) NamespaceKey { return continuousKey(common.NamespaceRating, n) }
	base := postingTable{
		keys:     make([]NamespaceKey, baseCount),
		postings: make([]posting, baseCount),
	}
	for n := range base.keys {
		base.keys[n] = key(int64(n * 2))
		base.postings[n] = makePosting(roar.FromUnsorted([]uint64{uint64(n + 1)}))
	}
	delta := &namespaceDelta{
		keys:   make([]NamespaceKey, deltaCount),
		values: make(map[NamespaceKey]*bitmapDelta, deltaCount),
	}
	for n := range delta.keys {
		k := key(int64(n*2 + 1))
		delta.keys[n] = k
		delta.values[k] = newBitmapDelta([]ImageID{ImageID(baseCount + n + 1)}, nil)
	}
	view := &namespaceView{base: base}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		sinkTable = view.effectiveTable(delta)
	}
}
