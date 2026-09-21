package hashidx

import (
	"math/rand"
	"slices"
	"sync"
	"testing"
)

// checkOverlayInvariants enforces the staging contract on a view: sorted
// unique sets, staged adds absent from base, staged removes present in
// base, and disjoint sides.
func checkOverlayInvariants(view *exactIndexView) error {
	o := view.overlay
	if o == nil {
		return nil
	}
	if !slices.IsSortedFunc(o.adds, compareEntry) {
		return errf("overlay adds unsorted")
	}
	if !slices.IsSortedFunc(o.removes, compareEntry) {
		return errf("overlay removes unsorted")
	}
	for k := 1; k < len(o.adds); k++ {
		if o.adds[k] == o.adds[k-1] {
			return errf("overlay adds duplicate %+v", o.adds[k])
		}
	}
	for k := 1; k < len(o.removes); k++ {
		if o.removes[k] == o.removes[k-1] {
			return errf("overlay removes duplicate %+v", o.removes[k])
		}
	}
	for _, e := range o.adds {
		if locatePair(view.blocks, e) >= 0 {
			return errf("overlay add present in base %+v", e)
		}
	}
	for _, e := range o.removes {
		if locatePair(view.blocks, e) < 0 {
			return errf("overlay remove absent from base %+v", e)
		}
	}
	if len(intersectSortedEntries(o.adds, o.removes)) != 0 {
		return errf("overlay sides overlap")
	}
	return nil
}

func overlayDepth(t *testing.T, idx *ExactIndex) (base, overlay uint64) {
	t.Helper()
	md := idx.Metadata()
	return md.BaseCount, md.OverlayCount
}

func TestOverlayStagesReadsBeforeFold(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 100, 1), ExactConfig{BlockSize: 512})
	beforeBlocks := idx.Metadata().BlockCount
	h := makeHash(1_000_001)
	if err := idx.Add(h, 50_001); err != nil {
		t.Fatal(err)
	}
	if !idx.Contains(h) {
		t.Fatal("staged add invisible to Contains")
	}
	if got := idx.Lookup(nil, h); len(got) != 1 || got[0] != 50_001 {
		t.Fatalf("staged Lookup = %v", got)
	}
	if got := idx.Count(h); got != 1 {
		t.Fatalf("staged Count = %d", got)
	}
	if _, depth := overlayDepth(t, idx); depth == 0 {
		t.Fatal("expected staged overlay entries")
	}
	if idx.Metadata().BlockCount != beforeBlocks {
		t.Fatal("staging rewrote blocks")
	}
	if got := idx.LiveCount(); got != 101 {
		t.Fatalf("LiveCount = %d, want 101", got)
	}
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayRemoveCancelsStagedAdd(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 100, 1), ExactConfig{BlockSize: 512})
	h := makeHash(2_000_001)
	beforeBlocks := idx.Metadata().BlockCount
	if err := idx.Add(h, 60_001); err != nil {
		t.Fatal(err)
	}
	if err := idx.Remove(h, 60_001); err != nil {
		t.Fatal(err)
	}
	if idx.Contains(h) {
		t.Fatal("cancelled pair still visible")
	}
	if got := idx.Count(h); got != 0 {
		t.Fatalf("cancelled Count = %d", got)
	}
	if _, depth := overlayDepth(t, idx); depth != 0 {
		t.Fatalf("overlay depth = %d, want 0 after cancel", depth)
	}
	if idx.Metadata().BlockCount != beforeBlocks {
		t.Fatal("cancel touched blocks")
	}
	if got := idx.LiveCount(); got != 100 {
		t.Fatalf("LiveCount = %d, want 100", got)
	}
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayRemoveOfBaseThenReAdd(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 100, 1), ExactConfig{BlockSize: 512})
	victim := ExactEntry{Hash: makeHash(10), ImageID: 11}
	if !idx.Contains(victim.Hash) {
		t.Fatal("setup: victim absent")
	}
	// Stage a removal, then re-add before any fold: net state equals base.
	if err := idx.Remove(victim.Hash, victim.ImageID); err != nil {
		t.Fatal(err)
	}
	// The victim pair itself must be gone (other IDs may share the hash,
	// so check the pair, not the hash).
	got := idx.Lookup(nil, victim.Hash)
	for _, id := range got {
		if id == victim.ImageID {
			t.Fatal("staged remove still visible")
		}
	}
	if err := idx.Add(victim.Hash, victim.ImageID); err != nil {
		t.Fatal(err)
	}
	got = idx.Lookup(nil, victim.Hash)
	found := false
	for _, id := range got {
		if id == victim.ImageID {
			found = true
		}
	}
	if !found {
		t.Fatal("re-added pair invisible")
	}
	if _, depth := overlayDepth(t, idx); depth != 0 {
		t.Fatalf("overlay depth = %d, want 0 after cancel", depth)
	}
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayFoldThreshold(t *testing.T) {
	idx := mustNewExactIndex(t, nil, ExactConfig{BlockSize: 512, OverlayMaxEntries: 8})
	for i := 0; i < 20; i++ {
		if err := idx.Add(makeHash(uint64(5_000_000+i)), ImageID(70_000+i)); err != nil {
			t.Fatal(err)
		}
	}
	base, depth := overlayDepth(t, idx)
	if base != 18 || depth != 2 {
		t.Fatalf("base=%d overlay=%d, want 18/2", base, depth)
	}
	if got := idx.LiveCount(); got != 20 {
		t.Fatalf("LiveCount = %d, want 20", got)
	}
	for i := 0; i < 20; i++ {
		if !idx.Contains(makeHash(uint64(5_000_000 + i))) {
			t.Fatalf("folded entry %d invisible", i)
		}
	}
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
}

// TestOverlayFoldEquivalenceRandom keeps a map model beside the index
// while a tiny overlay threshold forces constant stage/fold churn, then
// compares the effective state pair by pair.
func TestOverlayFoldEquivalenceRandom(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 64, 1),
		ExactConfig{BlockSize: 512, OverlayMaxEntries: 16})
	model := make(map[ExactEntry]bool)
	var present []ExactEntry
	for _, e := range entriesForRange(0, 64, 1) {
		model[e] = true
		present = append(present, e)
	}
	var nextID ImageID = 10_000
	rng := rand.New(rand.NewSource(11))
	removeFromPresent := func(e ExactEntry) {
		for k, p := range present {
			if p == e {
				present[k] = present[len(present)-1]
				present = present[:len(present)-1]
				return
			}
		}
	}
	for round := 0; round < 60; round++ {
		var m Mutation
		for k := 0; k < 12; k++ {
			switch rng.Intn(4) {
			case 0: // fresh add
				e := ExactEntry{Hash: makeHash(uint64(80_000_000 + round*100 + k)), ImageID: nextID}
				nextID++
				m.Add = append(m.Add, e)
				model[e] = true
				present = append(present, e)
			case 1: // duplicate add of a live pair (no-op)
				if len(present) > 0 {
					e := present[rng.Intn(len(present))]
					m.Add = append(m.Add, e)
				}
			case 2: // remove a live pair
				if len(present) > 0 {
					e := present[rng.Intn(len(present))]
					m.Remove = append(m.Remove, e)
					delete(model, e)
					removeFromPresent(e)
				}
			default: // remove absent (no-op)
				m.Remove = append(m.Remove, ExactEntry{Hash: makeHash(uint64(90_000_000 + k)), ImageID: nextID + 5000})
			}
		}
		if err := idx.Apply(m); err != nil {
			t.Fatal(err)
		}
		if err := checkViewInvariants(t, idx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got := idx.LiveCount(); got != uint64(len(model)) {
			t.Fatalf("round %d: LiveCount=%d model=%d", round, got, len(model))
		}
	}
	for e := range model {
		if !idx.Contains(e.Hash) {
			t.Fatalf("model pair %+v invisible to Contains", e)
		}
		ids := idx.Lookup(nil, e.Hash)
		found := false
		for _, id := range ids {
			if id == e.ImageID {
				found = true
			}
		}
		if !found {
			t.Fatalf("model pair %+v invisible to Lookup (%v)", e, ids)
		}
		if got := idx.Count(e.Hash); got != uint64(len(ids)) {
			t.Fatalf("count mismatch for %x: %d vs %d", e.Hash[:8], got, len(ids))
		}
	}
}

func TestOverlayCheckpointReopen(t *testing.T) {
	dir := t.TempDir()
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	var want []ExactEntry
	for i := 0; i < 40; i++ {
		e := ExactEntry{Hash: makeHash(uint64(6_000_000 + i)), ImageID: ImageID(80_000 + i)}
		want = append(want, e)
	}
	if err := idx.Apply(Mutation{Add: want}); err != nil {
		t.Fatal(err)
	}
	if _, depth := overlayDepth(t, idx); depth == 0 {
		t.Fatal("expected staged overlay before checkpoint")
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	for _, e := range want {
		if !idx.Contains(e.Hash) {
			t.Fatalf("post-checkpoint pair %+v invisible", e)
		}
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.LiveCount(); got != uint64(len(want)) {
		t.Fatalf("reopen LiveCount = %d, want %d", got, len(want))
	}
	if md := reopened.Metadata(); md.OverlayCount != 0 {
		t.Fatalf("reopened overlay depth = %d, want 0", md.OverlayCount)
	}
	for _, e := range want {
		if ids := reopened.Lookup(nil, e.Hash); len(ids) != 1 || ids[0] != e.ImageID {
			t.Fatalf("reopen lookup %+v = %v", e, ids)
		}
	}
	if err := checkViewInvariants(t, reopened); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayWALReplay(t *testing.T) {
	dir := t.TempDir()
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	var want []ExactEntry
	for i := 0; i < 25; i++ {
		e := ExactEntry{Hash: makeHash(uint64(7_000_000 + i)), ImageID: ImageID(90_000 + i)}
		want = append(want, e)
	}
	if err := idx.Apply(Mutation{Add: want[:20]}); err != nil {
		t.Fatal(err)
	}
	// Remove some staged pairs, then close without checkpointing: reopen
	// must replay the tail into the identical effective state.
	if err := idx.Apply(Mutation{Remove: want[:5]}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.LiveCount(); got != 15 {
		t.Fatalf("replay LiveCount = %d, want 15", got)
	}
	for _, e := range want[5:20] {
		if !reopened.Contains(e.Hash) {
			t.Fatalf("replayed pair %+v invisible", e)
		}
	}
	for _, e := range want[:5] {
		if reopened.Contains(e.Hash) {
			t.Fatalf("removed pair %+v resurrected", e)
		}
	}
	if err := checkViewInvariants(t, reopened); err != nil {
		t.Fatal(err)
	}
}

func TestCompiledLookup(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 200, 1), ExactConfig{BlockSize: 512})
	h := makeHash(77)
	wantIDs := idx.Lookup(nil, h)
	c := idx.CompileLookup(h)
	if got := c.Contains(); got != (len(wantIDs) > 0) {
		t.Fatalf("Contains = %v, want %v", got, len(wantIDs) > 0)
	}
	if got := c.Count(); got != uint64(len(wantIDs)) {
		t.Fatalf("Count = %d, want %d", got, len(wantIDs))
	}
	if got := c.Lookup(nil); !slices.Equal(got, wantIDs) {
		t.Fatalf("Lookup = %v, want %v", got, wantIDs)
	}
	// A write bumps the generation and re-resolves exactly once.
	extra := ExactEntry{Hash: h, ImageID: 500_001}
	if err := idx.Add(extra.Hash, extra.ImageID); err != nil {
		t.Fatal(err)
	}
	if got := c.Count(); got != uint64(len(wantIDs))+1 {
		t.Fatalf("post-write Count = %d, want %d", got, len(wantIDs)+1)
	}
	if got := c.Lookup(nil); len(got) != len(wantIDs)+1 || got[len(got)-1] != extra.ImageID {
		t.Fatalf("post-write Lookup = %v", got)
	}
	// Miss handle stays a miss until the hash appears.
	miss := idx.CompileLookup(makeHash(123_456_789))
	if miss.Contains() || miss.Count() != 0 || len(miss.Lookup(nil)) != 0 {
		t.Fatal("miss handle non-empty")
	}
	if err := idx.Add(miss.hash, 600_001); err != nil {
		t.Fatal(err)
	}
	if !miss.Contains() || miss.Count() != 1 {
		t.Fatal("miss handle did not re-resolve")
	}
	// Closed and nil handles report empty.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Contains() || c.Count() != 0 || len(c.Lookup(nil)) != 0 {
		t.Fatal("closed index handle non-empty")
	}
	var nilLookup *CompiledLookup
	if nilLookup.Contains() || nilLookup.Count() != 0 || len(nilLookup.Lookup(nil)) != 0 {
		t.Fatal("nil handle non-empty")
	}
}

func TestOverlayConcurrentStress(t *testing.T) {
	idx := mustNewExactIndex(t, entriesForRange(0, 200, 1),
		ExactConfig{BlockSize: 512, OverlayMaxEntries: 32})
	const writers = 6
	const perWriter = 120
	var wg sync.WaitGroup
	errs := make(chan error, writers+2)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for k := 0; k < perWriter; k++ {
				e := ExactEntry{
					Hash:    makeHash(uint64(100_000_000 + w*10_000 + k)),
					ImageID: ImageID(200_000 + w*10_000 + k),
				}
				if err := idx.Add(e.Hash, e.ImageID); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	compiled := idx.CompileLookup(makeHash(3))
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < writers*perWriter; k++ {
			_ = idx.Contains(makeHash(uint64(k % 200)))
			_ = idx.LiveCount()
			_ = idx.Count(makeHash(uint64(k % 200)))
			_ = compiled.Contains()
			_ = compiled.Count()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A second handle over a changing index exercises concurrent
		// re-resolution under -race.
		other := idx.CompileLookup(makeHash(9_999_999))
		for k := 0; k < writers*perWriter; k++ {
			_ = other.Lookup(nil)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := idx.LiveCount(); got != 200+writers*perWriter {
		t.Fatalf("LiveCount = %d, want %d", got, 200+writers*perWriter)
	}
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
	if md := idx.Metadata(); md.OverlayCount > 32 {
		t.Fatalf("overlay depth %d exceeds bound", md.OverlayCount)
	}
}
