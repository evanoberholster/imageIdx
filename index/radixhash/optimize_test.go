package radixhash

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

func applyBatched(index *Index, entries []Entry, size int) error {
	for start := 0; start < len(entries); start += size {
		end := start + size
		if end > len(entries) {
			end = len(entries)
		}
		if err := index.Apply(entries[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// TestApplySmallBatchSizesIdempotent drives the normal ImageIdx batch sizes,
// then re-applies the same records and asserts nothing changes: no duplicate
// rows, no extra entries, and no generation publication for a no-op.
func TestApplySmallBatchSizesIdempotent(t *testing.T) {
	const total = 200
	for _, size := range []int{1, 8, 16, 32, 64} {
		index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 2048})
		entries := make([]Entry, 0, total)
		model := make(map[Hash][]ImageID)
		for i := 0; i < total; i++ {
			h := hashWithPrefix([]byte{byte(i % 4), byte((i / 4) % 4)}, uint64(i+1))
			entries = append(entries, Entry{Hash: h, ImageID: ImageID(i + 1)})
			model[h] = append(model[h], ImageID(i+1))
		}
		if err := applyBatched(index, entries, size); err != nil {
			t.Fatalf("Apply(size=%d): %v", size, err)
		}
		genBefore := index.Generation()
		entriesBefore := index.Metadata().Entries
		if err := applyBatched(index, entries, size); err != nil {
			t.Fatalf("re-Apply(size=%d): %v", size, err)
		}
		if got := index.Generation(); got != genBefore {
			t.Fatalf("idempotent re-apply(size=%d) advanced generation %d -> %d", size, genBefore, got)
		}
		if got := index.Metadata().Entries; got != entriesBefore {
			t.Fatalf("idempotent re-apply(size=%d) changed entries %d -> %d", size, entriesBefore, got)
		}
		for h, ids := range model {
			if got := index.Lookup(nil, h); !equalIDs(got, uniqueSorted(ids)) {
				t.Fatalf("Lookup(size=%d, %x) = %v, want %v", size, h[:4], got, uniqueSorted(ids))
			}
		}
		if err := index.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestApplyLargeParallelBatch drives a batch at or above parallelMinEntries
// across many root buckets so the fan-out path is exercised, then checks the
// result and idempotency.
func TestApplyLargeParallelBatch(t *testing.T) {
	index := newTestIndex(t, 64<<20, Config{PromotionBytes: 4096})
	defer index.Close()

	const total = 8192
	entries := make([]Entry, 0, total)
	model := make(map[Hash][]ImageID)
	for i := 0; i < total; i++ {
		h := hashWithPrefix([]byte{byte(i % 200), byte((i / 200) % 200)}, uint64(i+1))
		entries = append(entries, Entry{Hash: h, ImageID: ImageID(i + 1)})
		model[h] = append(model[h], ImageID(i+1))
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	genBefore := index.Generation()
	entriesBefore := index.Metadata().Entries
	if err := index.Apply(entries); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if got := index.Generation(); got != genBefore {
		t.Fatalf("idempotent large re-apply advanced generation %d -> %d", genBefore, got)
	}
	if got := index.Metadata().Entries; got != entriesBefore {
		t.Fatalf("idempotent large re-apply changed entries %d -> %d", entriesBefore, got)
	}
	for h, ids := range model {
		if got := index.Lookup(nil, h); !equalIDs(got, uniqueSorted(ids)) {
			t.Fatalf("Lookup(%x) = %v, want %v", h[:4], got, uniqueSorted(ids))
		}
	}
}

// TestCompactGenerationIndependentOfBatchSize verifies compaction is one
// logical publication: the generation advances by exactly one no matter how
// many internal Apply calls or which compaction batch size is used.
func TestCompactGenerationIndependentOfBatchSize(t *testing.T) {
	original := compactBatchEntries
	defer func() { compactBatchEntries = original }()

	for _, size := range []int{1, 7, 128, 4096} {
		compactBatchEntries = size
		index, expected := walkFixture(t, 1500)
		genBefore := index.Generation()
		if err := index.Compact(); err != nil {
			t.Fatalf("Compact(batch=%d): %v", size, err)
		}
		if got := index.Generation(); got != genBefore+1 {
			t.Fatalf("generation after Compact(batch=%d) = %d, want %d", size, got, genBefore+1)
		}
		if got := index.Metadata().Generation; got != genBefore+1 {
			t.Fatalf("metadata generation after Compact(batch=%d) = %d, want %d", size, got, genBefore+1)
		}
		if err := index.Compact(); err != nil {
			t.Fatalf("second Compact(batch=%d): %v", size, err)
		}
		if got := index.Generation(); got != genBefore+2 {
			t.Fatalf("generation after second Compact(batch=%d) = %d, want %d", size, got, genBefore+2)
		}
		if got := index.Metadata().Entries; got != uint64(len(expected)) {
			t.Fatalf("entries after Compact(batch=%d) = %d, want %d", size, got, len(expected))
		}
		if err := index.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestCompactAbortsOnCorruptLeaf proves a corrupt old tree makes Compact fail
// with ErrCorrupt and leaves the published index exactly as it was.
func TestCompactAbortsOnCorruptLeaf(t *testing.T) {
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 1024})
	defer index.Close()

	var entries []Entry
	for i := 0; i < 300; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{3, 3}, uint64(i+1)), ImageID: ImageID(i + 1)})
	}
	healthy := hashWithPrefix([]byte{9, 9}, 7)
	entries = append(entries, Entry{Hash: healthy, ImageID: 9999})
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	genBefore := index.Generation()
	entriesBefore := index.Metadata().Entries

	view := index.view.Load()
	ref, _, ok := index.traverse(view, healthy)
	if !ok {
		t.Fatalf("traverse(healthy) miss")
	}
	view.arena[extentOffset(ref)] = 0 // destroy the leaf magic

	if err := index.Compact(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact(corrupt leaf) = %v, want ErrCorrupt", err)
	}
	if got := index.Generation(); got != genBefore {
		t.Fatalf("generation changed on failed Compact: %d -> %d", genBefore, got)
	}
	if got := index.Metadata().Entries; got != entriesBefore {
		t.Fatalf("entries changed on failed Compact: %d -> %d", entriesBefore, got)
	}
	// The untouched bucket is still fully usable after the aborted rebuild.
	if got := index.Lookup(nil, hashWithPrefix([]byte{3, 3}, 1)); !equalIDs(got, []ImageID{1}) {
		t.Fatalf("healthy bucket after failed Compact = %v, want [1]", got)
	}
}

// TestCompactAbortsOnCorruptNode points an interior node child at a radix node
// outside the arena; the walker must report corruption rather than skip it.
func TestCompactAbortsOnCorruptNode(t *testing.T) {
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 1024})
	defer index.Close()

	var entries []Entry
	for i := 0; i < 300; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{3, 3}, uint64(i+1)), ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	view := index.view.Load()
	ref, isNode := decodeChild(view.root[3])
	if !isNode {
		t.Fatalf("bucket 3 is not a radix node after promotion")
	}
	// Overwrite the first child with a node reference far outside the arena.
	offset := extentOffset(ref)
	binary.LittleEndian.PutUint32(view.arena[offset:], encodeChild(pagealloc.Ref(0x0FFFFFFF), true))

	if err := index.Compact(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact(corrupt node) = %v, want ErrCorrupt", err)
	}
	if got := index.Metadata().Entries; got != uint64(len(entries)) {
		t.Fatalf("entries changed on failed Compact: %d, want %d", got, len(entries))
	}
}

// TestCompactReleaseErrorPropagates verifies a failing old-arena release is
// returned after the replacement is already published, not silently dropped.
func TestCompactReleaseErrorPropagates(t *testing.T) {
	index := newTestIndex(t, 4<<20, Config{PromotionBytes: 1024})
	defer index.Close()
	var entries []Entry
	for i := 0; i < 300; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{3, 3}, uint64(i+1)), ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := errors.New("release failed")
	index.release = func() error { return want }
	genBefore := index.Generation()
	if err := index.Compact(); !errors.Is(err, want) {
		t.Fatalf("Compact(release error) = %v, want %v", err, want)
	}
	// The replacement is live despite the release error.
	if got := index.Generation(); got != genBefore+1 {
		t.Fatalf("generation after Compact = %d, want %d", got, genBefore+1)
	}
	if got := index.Lookup(nil, hashWithPrefix([]byte{3, 3}, 1)); !equalIDs(got, []ImageID{1}) {
		t.Fatalf("lookup after release-error Compact = %v, want [1]", got)
	}
}

// TestMergeSlotsLinearDedup exercises the linear merge against duplicates in
// both the existing leaf and the incoming group.
func TestMergeSlotsLinearDedup(t *testing.T) {
	tailLen := tailLenFor(1)
	mk := func(seed uint64, id uint64) slot {
		var s slot
		h := hashWithPrefix([]byte{1}, seed)
		copy(s.tail[:], h[1:])
		s.id = id
		return s
	}
	existing := []slot{mk(1, 1), mk(2, 2), mk(4, 4)}
	added := []slot{mk(4, 4), mk(2, 2), mk(3, 3), mk(3, 3), mk(5, 5)}
	merged := mergeSlots(existing, added, tailLen)
	if len(merged) != 5 {
		t.Fatalf("merged length = %d, want 5", len(merged))
	}
	for i := 1; i < len(merged); i++ {
		if compareSlot(&merged[i-1], &merged[i], tailLen) >= 0 {
			t.Fatalf("merged not strictly ascending at %d", i)
		}
	}
}
