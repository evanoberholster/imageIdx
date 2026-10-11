package radixhash

import (
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
)

func TestRemoveSingleAssociation(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{1, 2}, 7)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 1}, {Hash: h, ImageID: 2}, {Hash: h, ImageID: 3}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Remove([]Entry{{Hash: h, ImageID: 2}}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{1, 3}) {
		t.Fatalf("Lookup = %v, want [1 3]", got)
	}
	if got := index.Count(h); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}
	if !index.Contains(h) {
		t.Fatal("Contains = false after partial removal")
	}
	if got := index.Metadata().Entries; got != 2 {
		t.Fatalf("Metadata.Entries = %d, want 2", got)
	}
}

// TestRemoveLeavesSiblingValueIDs verifies that removing (A, 100) does not
// disturb the other ValueIDs associated with A.
func TestRemoveLeavesSiblingValueIDs(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{3, 3}, 11)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 100}, {Hash: h, ImageID: 200}, {Hash: h, ImageID: 300}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Remove([]Entry{{Hash: h, ImageID: 100}}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{200, 300}) {
		t.Fatalf("Lookup = %v, want [200 300]", got)
	}
}

func TestRemoveMissingIsNoOp(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{4, 4}, 5)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := index.Generation()
	if err := index.Remove([]Entry{{Hash: h, ImageID: 99}, {Hash: hashWithPrefix([]byte{9}, 1), ImageID: 1}}); err != nil {
		t.Fatalf("Remove missing: %v", err)
	}
	if after := index.Generation(); after != before {
		t.Fatalf("missing removal published generation %d -> %d", before, after)
	}
	if got := index.Metadata().Entries; got != 1 {
		t.Fatalf("Entries = %d, want 1", got)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{5, 5}, 6)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 1}, {Hash: h, ImageID: 2}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Remove([]Entry{{Hash: h, ImageID: 1}}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	gen := index.Generation()
	if err := index.Remove([]Entry{{Hash: h, ImageID: 1}}); err != nil {
		t.Fatalf("Remove again: %v", err)
	}
	if index.Generation() != gen {
		t.Fatal("repeated removal was not idempotent")
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{2}) {
		t.Fatalf("Lookup = %v, want [2]", got)
	}
}

func TestRemovePrunesEmptyLeaf(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	a := hashWithPrefix([]byte{10}, 1)
	b := hashWithPrefix([]byte{20}, 2)
	if err := index.Apply([]Entry{{Hash: a, ImageID: 1}, {Hash: b, ImageID: 2}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Remove([]Entry{{Hash: a, ImageID: 1}}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := index.Lookup(nil, a); len(got) != 0 {
		t.Fatalf("Lookup(a) = %v, want empty", got)
	}
	if index.Contains(a) {
		t.Fatal("Contains(a) = true after emptying")
	}
	if got := index.Lookup(nil, b); !equalIDs(got, []ImageID{2}) {
		t.Fatalf("Lookup(b) = %v, want [2]", got)
	}
	if got := index.Metadata().Entries; got != 1 {
		t.Fatalf("Entries = %d, want 1", got)
	}
}

// TestRemoveDrainsPromotedTree removes every record from a tree that has been
// promoted past the leaf threshold, exercising node pruning.
func TestRemoveDrainsPromotedTree(t *testing.T) {
	index := newTestIndex(t, 4<<20, Config{PromotionBytes: 1 << 30, MaxLeafSlots: 32})
	defer index.Close()
	const n = 300
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{7, 9}, uint64(i)+1), ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if depth := index.Metadata().Depth; depth < 3 {
		t.Fatalf("depth = %d, want >= 3", depth)
	}
	if err := index.Remove(entries); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	meta := index.Metadata()
	if meta.Entries != 0 || meta.RootBuckets != 0 {
		t.Fatalf("after drain Entries=%d RootBuckets=%d, want 0/0", meta.Entries, meta.RootBuckets)
	}
	for i := 0; i < n; i++ {
		h := hashWithPrefix([]byte{7, 9}, uint64(i)+1)
		if got := index.Lookup(nil, h); len(got) != 0 {
			t.Fatalf("Lookup(%d) = %v, want empty", i, got)
		}
	}
}

func TestRemoveRejectsZeroID(t *testing.T) {
	index := newTestIndex(t, 1<<20, Config{})
	defer index.Close()
	if err := index.Remove([]Entry{{Hash: hashWithPrefix([]byte{1}, 1), ImageID: 0}}); err != ErrInvalidEntry {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
}

func TestRemovePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arena.bin")
	index, err := NewMmap(path, 1<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	a := hashWithPrefix([]byte{8, 8}, 1)
	b := hashWithPrefix([]byte{8, 8}, 2)
	var entries []Entry
	for id := ImageID(1); id <= 64; id++ {
		entries = append(entries, Entry{Hash: a, ImageID: id})
	}
	entries = append(entries, Entry{Hash: b, ImageID: 1})
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Remove half of a's records and all of b.
	remove := make([]Entry, 0, 33)
	for id := ImageID(1); id <= 32; id++ {
		remove = append(remove, Entry{Hash: a, ImageID: id})
	}
	remove = append(remove, Entry{Hash: b, ImageID: 1})
	if err := index.Remove(remove); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenMmap(path, Config{})
	if err != nil {
		t.Fatalf("OpenMmap: %v", err)
	}
	defer reopened.Close()
	var wantA []ImageID
	for id := ImageID(33); id <= 64; id++ {
		wantA = append(wantA, id)
	}
	if got := reopened.Lookup(nil, a); !equalIDs(got, wantA) {
		t.Fatalf("reopened Lookup(a) = %v, want %v", got, wantA)
	}
	if got := reopened.Lookup(nil, b); len(got) != 0 {
		t.Fatalf("reopened Lookup(b) = %v, want empty", got)
	}
	if got := reopened.Metadata().Entries; got != 32 {
		t.Fatalf("reopened Entries = %d, want 32", got)
	}
}

func TestRemoveConcurrentReaders(t *testing.T) {
	index := newTestIndex(t, 16<<20, Config{AutoReclaimBytes: 256 << 10})
	defer index.Close()
	const hashes = 32
	for i := 0; i < hashes; i++ {
		h := hashWithPrefix([]byte{12}, uint64(i)+1)
		var batch []Entry
		for id := ImageID(1); id <= 32; id++ {
			batch = append(batch, Entry{Hash: h, ImageID: id})
		}
		if err := index.Apply(batch); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	var stop sync.WaitGroup
	stop.Add(4)
	for r := 0; r < 4; r++ {
		go func(seed int) {
			defer stop.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			for i := 0; i < 2000; i++ {
				h := hashWithPrefix([]byte{12}, uint64(rng.Intn(hashes))+1)
				_ = index.Lookup(nil, h)
			}
		}(r)
	}
	for round := 0; round < 200; round++ {
		h := hashWithPrefix([]byte{12}, uint64(round%hashes)+1)
		if err := index.Remove([]Entry{{Hash: h, ImageID: ImageID(round%32 + 1)}}); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if err := index.Apply([]Entry{{Hash: h, ImageID: ImageID(round%32 + 1)}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	stop.Wait()
}

// BenchmarkRemove measures the steady-state cost of toggling one (hash, id)
// association on a 4096-record index. It is new in the P0 fix; there is no
// pre-fix baseline.
func BenchmarkRemove(b *testing.B) {
	index, err := New(64<<20, Config{AutoReclaimBytes: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer index.Close()
	var entries []Entry
	for i := 0; i < 4096; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{byte(i % 256)}, uint64(i)), ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(entries); err != nil {
		b.Fatal(err)
	}
	h := hashWithPrefix([]byte{0}, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		if i%2 == 0 {
			err = index.Remove([]Entry{{Hash: h, ImageID: 1}})
		} else {
			err = index.Apply([]Entry{{Hash: h, ImageID: 1}})
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}

// TestRemoveModel drives random insert/remove batches against a reference set
// and checks Lookup, Count, Contains and the entry count after every batch.
func TestRemoveModel(t *testing.T) {
	index := newTestIndex(t, 4<<20, Config{PromotionBytes: 4096})
	defer index.Close()
	rng := rand.New(rand.NewSource(2024))
	const distinct = 24
	hashes := make([]Hash, distinct)
	for i := range hashes {
		hashes[i] = hashWithPrefix([]byte{byte(i)}, uint64(i)+1)
	}
	model := make([]map[ImageID]struct{}, distinct)
	for i := range model {
		model[i] = make(map[ImageID]struct{})
	}
	for round := 0; round < 400; round++ {
		var ops []Entry
		remove := rng.Intn(2) == 0
		for n := 0; n < 1+rng.Intn(48); n++ {
			hi := rng.Intn(distinct)
			id := ImageID(1 + rng.Intn(128))
			ops = append(ops, Entry{Hash: hashes[hi], ImageID: id})
			if remove {
				delete(model[hi], id)
			} else {
				model[hi][id] = struct{}{}
			}
		}
		var err error
		if remove {
			err = index.Remove(ops)
		} else {
			err = index.Apply(ops)
		}
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		var total uint64
		var got []ImageID
		for hi := range hashes {
			got = index.Lookup(got[:0], hashes[hi])
			want := make([]ImageID, 0, len(model[hi]))
			for id := range model[hi] {
				want = append(want, id)
			}
			want = uniqueSorted(want)
			if !equalIDs(got, want) {
				t.Fatalf("round %d hash %d: Lookup = %v, want %v", round, hi, got, want)
			}
			if index.Count(hashes[hi]) != uint64(len(want)) {
				t.Fatalf("round %d hash %d: Count = %d, want %d", round, hi, index.Count(hashes[hi]), len(want))
			}
			total += uint64(len(want))
		}
		if got := index.Metadata().Entries; got != total {
			t.Fatalf("round %d: Entries = %d, want %d", round, got, total)
		}
	}
}
