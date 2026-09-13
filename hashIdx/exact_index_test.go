package hashidx

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
)

func makeHash(n uint64) Hash {
	var h Hash
	h[0] = byte(n >> 56)
	h[1] = byte(n >> 48)
	h[2] = byte(n >> 40)
	h[3] = byte(n >> 32)
	h[4] = byte(n >> 24)
	h[5] = byte(n >> 16)
	h[6] = byte(n >> 8)
	h[7] = byte(n)
	return h
}

func mustNewExactIndex(t *testing.T, entries []ExactEntry, cfg ExactConfig) *ExactIndex {
	t.Helper()
	if cfg.BlockSize == 0 && cfg.TargetFillPct == 0 && cfg.MaxBatchEntries == 0 {
		// leave zero to exercise defaults unless caller overrides
	}
	idx, err := newExact(t.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, cfg, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func entriesForRange(start, count uint64, idBase ImageID) []ExactEntry {
	out := make([]ExactEntry, 0, count)
	for i := uint64(0); i < count; i++ {
		out = append(out, ExactEntry{Hash: makeHash(start + i), ImageID: idBase + ImageID(i)})
	}
	return out
}

func checkAscendingUnique(t *testing.T, ids []ImageID) {
	t.Helper()
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("ids not strictly ascending at %d: %v", i, ids)
		}
	}
}

func TestEmptyIndex(t *testing.T) {
	idx := mustNewExactIndex(t, nil, ExactConfig{})
	h := makeHash(1)
	if idx.Contains(h) {
		t.Fatal("empty index contains hash")
	}
	if got := idx.Lookup(nil, h); len(got) != 0 {
		t.Fatalf("empty lookup = %v", got)
	}
	if n := idx.Count(h); n != 0 {
		t.Fatalf("empty count = %d", n)
	}
	md := idx.Metadata()
	if md.BaseCount != 0 || md.BlockCount != 0 {
		t.Fatalf("empty metadata = %+v", md)
	}
}

func TestSingleBlockLookup(t *testing.T) {
	entries := entriesForRange(10, 20, 100)
	idx := mustNewExactIndex(t, entries, ExactConfig{BlockSize: 16384})
	// first / middle / last
	for _, tc := range []struct {
		hash Hash
		want ImageID
	}{
		{makeHash(10), 100},
		{makeHash(19), 109},
		{makeHash(29), 119},
	} {
		if !idx.Contains(tc.hash) {
			t.Fatalf("missing hash %x", tc.hash[:8])
		}
		got := idx.Lookup(nil, tc.hash)
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("lookup = %v, want [%d]", got, tc.want)
		}
		if n := idx.Count(tc.hash); n != 1 {
			t.Fatalf("count = %d, want 1", n)
		}
	}
	// missing hashes before, between is impossible (dense), after
	for _, miss := range []Hash{makeHash(9), makeHash(30), makeHash(999)} {
		if idx.Contains(miss) {
			t.Fatalf("false positive for %x", miss[:8])
		}
		if got := idx.Lookup(nil, miss); len(got) != 0 {
			t.Fatalf("miss lookup = %v", got)
		}
	}
}

func TestMultiBlockLookup(t *testing.T) {
	// Tiny blocks force many blocks.
	entries := entriesForRange(0, 50, 1)
	idx := mustNewExactIndex(t, entries, ExactConfig{BlockSize: 256, TargetFillPct: 80})
	md := idx.Metadata()
	if md.BlockCount < 3 {
		t.Fatalf("block count = %d, want >=3 with tiny blocks", md.BlockCount)
	}
	for i := uint64(0); i < 50; i++ {
		h := makeHash(i)
		got := idx.Lookup(nil, h)
		if len(got) != 1 || got[0] != ImageID(i+1) {
			t.Fatalf("hash %d lookup = %v", i, got)
		}
	}
}

func TestOneHashMultipleIDs(t *testing.T) {
	h := makeHash(42)
	entries := []ExactEntry{
		{Hash: h, ImageID: 900},
		{Hash: h, ImageID: 12},
		{Hash: h, ImageID: 41},
	}
	idx := mustNewExactIndex(t, entries, ExactConfig{})
	got := idx.Lookup(nil, h)
	if len(got) != 3 || got[0] != 12 || got[1] != 41 || got[2] != 900 {
		t.Fatalf("lookup = %v, want [12 41 900]", got)
	}
	checkAscendingUnique(t, got)
	if !idx.Contains(h) || idx.Count(h) != 3 {
		t.Fatal("contains/count mismatch for multi-ID hash")
	}
}

func TestDuplicateRunSpanningBlocks(t *testing.T) {
	h := makeHash(7)
	var entries []ExactEntry
	for id := ImageID(1); id <= 20; id++ {
		entries = append(entries, ExactEntry{Hash: h, ImageID: id})
	}
	// Prepend and append neighboring hashes to force boundary conditions.
	entries = append(entries, ExactEntry{Hash: makeHash(6), ImageID: 1000})
	entries = append(entries, ExactEntry{Hash: makeHash(8), ImageID: 1001})
	idx := mustNewExactIndex(t, entries, ExactConfig{BlockSize: 256, TargetFillPct: 80})
	if idx.Metadata().BlockCount < 2 {
		t.Fatalf("expected spanning blocks, got %d", idx.Metadata().BlockCount)
	}
	got := idx.Lookup(nil, h)
	if len(got) != 20 {
		t.Fatalf("spanning lookup len = %d, want 20", len(got))
	}
	checkAscendingUnique(t, got)
	for i, id := range got {
		if id != ImageID(i+1) {
			t.Fatalf("spanning ids[%d] = %d", i, id)
		}
	}
	if n := idx.Count(h); n != 20 {
		t.Fatalf("spanning count = %d", n)
	}
}

func TestInsertPositions(t *testing.T) {
	initial := []ExactEntry{
		{Hash: makeHash(10), ImageID: 1},
		{Hash: makeHash(20), ImageID: 2},
		{Hash: makeHash(30), ImageID: 3},
		{Hash: makeHash(40), ImageID: 4},
	}
	idx := mustNewExactIndex(t, initial, ExactConfig{BlockSize: 512})
	// start
	if err := idx.Add(makeHash(1), 100); err != nil {
		t.Fatal(err)
	}
	// middle (new hash between 20 and 30)
	if err := idx.Add(makeHash(25), 101); err != nil {
		t.Fatal(err)
	}
	// end
	if err := idx.Add(makeHash(99), 102); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		h    Hash
		want ImageID
	}{
		{makeHash(1), 100}, {makeHash(25), 101}, {makeHash(99), 102},
	} {
		got := idx.Lookup(nil, tc.h)
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("lookup %x = %v", tc.h[:8], got)
		}
	}
}

func TestBatchIntoOneBlockRewritesOnce(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 10, 1), ExactConfig{BlockSize: 4096})
	before := idx.Metadata().Generation
	var adds []ExactEntry
	for i := uint64(100); i < 110; i++ {
		// Hashes routing to the same neighborhood (all greater than existing
		// to land in the last block).
		adds = append(adds, ExactEntry{Hash: makeHash(1000 + i), ImageID: ImageID(i)})
	}
	if err := idx.Apply(Mutation{Add: adds}); err != nil {
		t.Fatal(err)
	}
	after := idx.Metadata().Generation
	if after != before+1 {
		t.Fatalf("batch published %d times, want once (gen %d -> %d)", after-before, before, after)
	}
	for _, e := range adds {
		got := idx.Lookup(nil, e.Hash)
		if len(got) != 1 || got[0] != e.ImageID {
			t.Fatalf("batch entry missing: %+v -> %v", e, got)
		}
	}
}

func TestBatchTouchingManyBlocks(t *testing.T) {
	entries := entriesForRange(0, 100, 1)
	idx := mustNewExactIndex(t, entries, ExactConfig{BlockSize: 512})
	var adds []ExactEntry
	for i := uint64(0); i < 100; i += 10 {
		adds = append(adds, ExactEntry{Hash: makeHash(i), ImageID: 1000 + ImageID(i)})
	}
	if err := idx.Apply(Mutation{Add: adds}); err != nil {
		t.Fatal(err)
	}
	for _, e := range adds {
		got := idx.Lookup(nil, e.Hash)
		if len(got) != 2 {
			t.Fatalf("hash %x lookup = %v, want 2 ids", e.Hash[:8], got)
		}
		checkAscendingUnique(t, got)
	}
}

func TestSplitAroundDuplicates(t *testing.T) {
	// Build blocks then overflow one block whose run should not split mid-hash
	// when a clean boundary exists.
	var entries []ExactEntry
	for i := uint64(0); i < 10; i++ {
		entries = append(entries, ExactEntry{Hash: makeHash(i), ImageID: ImageID(i + 1)})
	}
	idx := mustNewExactIndex(t, entries, ExactConfig{BlockSize: 256, TargetFillPct: 80})
	// Add several IDs under one existing hash plus neighboring hashes to
	// force a split decision.
	h := makeHash(5)
	var adds []ExactEntry
	for id := ImageID(100); id < 110; id++ {
		adds = append(adds, ExactEntry{Hash: h, ImageID: id})
	}
	if err := idx.Apply(Mutation{Add: adds}); err != nil {
		t.Fatal(err)
	}
	got := idx.Lookup(nil, h)
	if len(got) != 11 { // 1 original + 10 new
		t.Fatalf("split lookup len = %d, want 11: %v", len(got), got)
	}
	checkAscendingUnique(t, got)
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAndRemoveWins(t *testing.T) {
	entries := entriesForRange(0, 10, 1)
	idx := mustNewExactIndex(t, entries, ExactConfig{})
	h := makeHash(3)
	if err := idx.Remove(h, 4); err != nil { // makeHash(3) has ID 4
		t.Fatal(err)
	}
	if idx.Contains(h) {
		// hash 3 had exactly one ID, now removed
		if n := idx.Count(h); n != 0 {
			t.Fatalf("count after delete = %d", n)
		}
	}
	// add + remove same pair in one mutation: remove wins
	e := ExactEntry{Hash: makeHash(50), ImageID: 500}
	if err := idx.Apply(Mutation{Add: []ExactEntry{e}, Remove: []ExactEntry{e}}); err != nil {
		t.Fatal(err)
	}
	if got := idx.Lookup(nil, e.Hash); len(got) != 0 {
		t.Fatalf("remove-wins violated: %v", got)
	}
	// remove existing + re-add same pair in one mutation: remove wins, so the
	// pair ends absent.
	existing := ExactEntry{Hash: makeHash(4), ImageID: 5}
	if err := idx.Apply(Mutation{Add: []ExactEntry{existing}, Remove: []ExactEntry{existing}}); err != nil {
		t.Fatal(err)
	}
	if got := idx.Lookup(nil, existing.Hash); len(got) != 0 {
		t.Fatalf("existing remove-wins violated: %v", got)
	}
}

func TestNoDuplicateAfterRepeatedAdd(t *testing.T) {
	idx := mustNewExactIndex(t, nil, ExactConfig{})
	e := ExactEntry{Hash: makeHash(1), ImageID: 1}
	for i := 0; i < 3; i++ {
		if err := idx.Add(e.Hash, e.ImageID); err != nil {
			t.Fatal(err)
		}
	}
	if n := idx.Count(e.Hash); n != 1 {
		t.Fatalf("count after repeated adds = %d, want 1", n)
	}
}

func TestRejectsDuplicateSpecAndBadEntries(t *testing.T) {
	dup := []ExactEntry{{Hash: makeHash(1), ImageID: 1}, {Hash: makeHash(1), ImageID: 1}}
	if _, err := newExact(t.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: dup}, ExactConfig{}, DurabilityOS); err == nil {
		t.Fatal("expected duplicate entry error")
	}
	bad := []ExactEntry{{Hash: makeHash(1), ImageID: 0}}
	if _, err := newExact(t.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: bad}, ExactConfig{}, DurabilityOS); err == nil {
		t.Fatal("expected invalid entry error for ImageID 0")
	}
	if _, err := newExact(t.TempDir(), ExactSpec{Algorithm: 0}, ExactConfig{}, DurabilityOS); err == nil {
		t.Fatal("expected invalid algorithm error")
	}
	if err := mustNewExactIndex(t, nil, ExactConfig{}).Apply(Mutation{Add: []ExactEntry{{Hash: makeHash(1), ImageID: 0}}}); err == nil {
		t.Fatal("expected invalid entry on Apply")
	}
}

func TestBatchTooLarge(t *testing.T) {
	idx := mustNewExactIndex(t, nil, ExactConfig{MaxBatchEntries: 4})
	var adds []ExactEntry
	for i := 0; i < 5; i++ {
		adds = append(adds, ExactEntry{Hash: makeHash(uint64(i)), ImageID: ImageID(i + 1)})
	}
	if err := idx.Apply(Mutation{Add: adds}); err == nil {
		t.Fatal("expected batch-too-large error")
	}
}

func TestExactConcurrentReadersAndWriter(t *testing.T) {
	entries := entriesForRange(0, 200, 1)
	idx := mustNewExactIndex(t, entries, ExactConfig{BlockSize: 1024})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var dst []ImageID
			for {
				select {
				case <-stop:
					return
				default:
					dst = idx.Lookup(dst[:0], makeHash(50))
					if len(dst) < 1 {
						// Writer may have deleted? No deletes here, so must
						// always find at least the original.
						panic("lost entry under concurrent read")
					}
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		_ = idx.Add(makeHash(uint64(1000+i)), ImageID(10000+i))
	}
	close(stop)
	wg.Wait()
}

func TestReaderHoldsOldViewAcrossPublish(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 10, 1), ExactConfig{})
	token := idx.readers.Acquire()
	old := idx.view.Load()
	h := makeHash(500)
	if err := idx.Add(h, 999); err != nil {
		idx.readers.Release(token)
		t.Fatal(err)
	}
	// Old view must not contain the new pair; new view must.
	if viewLookup(nil, old, h) != nil && len(viewLookup(nil, old, h)) != 0 {
		idx.readers.Release(token)
		t.Fatal("old view mutated by publish")
	}
	if len(idx.Lookup(nil, h)) != 1 {
		idx.readers.Release(token)
		t.Fatal("new view missing published entry")
	}
	idx.readers.Release(token)
}

func TestSteadyStateLookupAllocs(t *testing.T) {
	entries := entriesForRange(0, 1024, 1)
	idx := mustNewExactIndex(t, entries, ExactConfig{})
	h := makeHash(512)
	dst := make([]ImageID, 0, 8)
	if allocs := testing.AllocsPerRun(1000, func() {
		dst = idx.Lookup(dst[:0], h)
	}); allocs != 0 {
		t.Fatalf("Lookup allocs = %f, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		_ = idx.Contains(h)
	}); allocs != 0 {
		t.Fatalf("Contains allocs = %f, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		_ = idx.Count(h)
	}); allocs != 0 {
		t.Fatalf("Count allocs = %f, want 0", allocs)
	}
}

func TestInvariantsAfterRandomMutations(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 50, 1), ExactConfig{BlockSize: 512})
	for round := 0; round < 20; round++ {
		var adds, removes []ExactEntry
		for k := 0; k < 10; k++ {
			adds = append(adds, ExactEntry{Hash: makeHash(uint64((round*37 + k*11) % 80)), ImageID: ImageID(round*100 + k + 1)})
		}
		if round%3 == 0 {
			removes = append(removes, ExactEntry{Hash: makeHash(uint64(round % 50)), ImageID: ImageID(round%50 + 1)})
		}
		if err := idx.Apply(Mutation{Add: adds, Remove: removes}); err != nil {
			t.Fatal(err)
		}
		if err := checkViewInvariants(t, idx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

func checkViewInvariants(t *testing.T, idx *ExactIndex) error {
	t.Helper()
	view := idx.view.Load()
	var prevHash *Hash
	seen := make(map[ExactEntry]struct{})
	var total uint64
	for bi, b := range view.blocks {
		if len(b.records)%RecordSize != 0 {
			t.Fatalf("block %d bad length", bi)
		}
		if !recordsSorted(b.records) {
			return errf("block %d records unsorted", bi)
		}
		n := b.count()
		if n == 0 {
			return errf("block %d empty", bi)
		}
		var first Hash
		copy(first[:], b.records[:32])
		if first != b.first {
			return errf("block %d FirstHash mismatch", bi)
		}
		if prevHash != nil && compareHash(*prevHash, b.first) > 0 {
			return errf("directory unsorted at block %d", bi)
		}
		h := b.first
		prevHash = &h
		for k := 0; k < n; k++ {
			e := decodeRecord(b.records[k*RecordSize : (k+1)*RecordSize])
			if _, dup := seen[e]; dup {
				return errf("duplicate pair %+v", e)
			}
			seen[e] = struct{}{}
		}
		total += uint64(n)
	}
	if total != view.liveCount {
		return errf("liveCount %d != actual %d", view.liveCount, total)
	}
	// Global order: concatenating blocks must be sorted.
	var all []ExactEntry
	for _, b := range view.blocks {
		all = append(all, decodeBlockEntries(b.records)...)
	}
	for k := 1; k < len(all); k++ {
		if compareEntry(all[k-1], all[k]) >= 0 {
			return errf("global order violated at %d", k)
		}
	}
	// Lookup/Count consistency spot-check.
	for _, e := range all {
		ids := viewLookup(nil, view, e.Hash)
		found := false
		for _, id := range ids {
			if id == e.ImageID {
				found = true
				break
			}
		}
		if !found {
			return errf("lookup missed %+v", e)
		}
		if uint64(len(ids)) != viewCount(view, e.Hash) {
			return errf("count mismatch for %x", e.Hash[:8])
		}
	}
	_ = bytes.Compare
	_ = context.Background
	return nil
}

type testError string

func (e testError) Error() string { return string(e) }

func errf(format string, args ...interface{}) error {
	return testError(fmt.Sprintf(format, args...))
}

func TestAlgorithmsBehaveEquivalently(t *testing.T) {
	payloads := []string{"alpha", "beta", "alpha", "gamma"}
	for _, algo := range []Algorithm{AlgorithmSHA256, AlgorithmBLAKE3} {
		sum := SumSHA256
		if algo == AlgorithmBLAKE3 {
			sum = SumBLAKE3
		}
		idx, err := newExact(t.TempDir(), ExactSpec{Algorithm: algo}, ExactConfig{BlockSize: 512}, DurabilityOS)
		if err != nil {
			t.Fatal(err)
		}
		id := ImageID(1)
		for _, p := range payloads {
			h := sum([]byte(p))
			if err := idx.Add(h, id); err != nil {
				t.Fatal(err)
			}
			id++
		}
		// "alpha" was added twice with different IDs.
		alphaIDs := idx.Lookup(nil, sum([]byte("alpha")))
		if len(alphaIDs) != 2 || alphaIDs[0] == alphaIDs[1] {
			t.Fatalf("%s alpha ids = %v", algo, alphaIDs)
		}
		checkAscendingUnique(t, alphaIDs)
		if n := idx.Count(sum([]byte("beta"))); n != 1 {
			t.Fatalf("%s beta count = %d", algo, n)
		}
		if idx.Count(sum([]byte("missing"))) != 0 {
			t.Fatalf("%s missing count != 0", algo)
		}
		_ = idx.Close()
	}
}
