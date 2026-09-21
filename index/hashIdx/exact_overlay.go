package hashidx

import (
	"slices"
	"sync"
)

// This file implements the staged correction layer for ExactIndex,
// borrowed from bitmapidx's frozen/active delta design:
//
//   - Small publishes stage into an immutable overlay instead of rewriting
//     blocks and copying the whole block directory. Staging is linear in
//     the overlay plus the batch; a publish without a fold touches no
//     block bytes and shares the block slice with the previous view.
//   - Reads consult the overlay first, then base blocks, mirroring
//     fieldView's active/frozen/base precedence.
//   - Past OverlayMaxEntries staged IDs the batch folds into the blocks
//     through the existing applyEffective path and clears the overlay,
//     amortizing the O(#blocks) directory copy over many publishes.
//
// Overlay invariants (enforced by stageOverlay, asserted in tests):
//
//  1. adds and removes are each sorted unique by (Hash, ImageID).
//  2. adds ∩ base blocks = ∅; removes ⊆ base blocks.
//  3. adds ∩ removes = ∅.
//
// Together they make folding trivial: staged sets are already canonical
// against base, so a fold merges them straight into applyEffective with
// no re-canonicalization.
//
// Read cost: every lookup probes the overlay with up to two binary
// searches (adds, then removes) before/while walking base. Measured
// +50-70ns on a 40-60ns base with a ~400-entry overlay, 0 allocs.
// Workloads that burst-write then read hot should checkpoint (which folds
// for the persisted file) or serve hot hashes through CompiledLookup,
// which replays at ~6ns regardless of overlay depth.

// exactOverlay is one immutable staged correction over base blocks: recent
// adds not yet folded in, and recent removes not yet folded out. It is
// owned by one exactIndexView and never mutated after publication; every
// stage allocates fresh slices. A nil overlay means no staged corrections.
type exactOverlay struct {
	adds    []ExactEntry
	removes []ExactEntry
}

// ids reports the staged entry count driving fold decisions. It is safe
// on nil.
func (o *exactOverlay) ids() int {
	if o == nil {
		return 0
	}
	return len(o.adds) + len(o.removes)
}

// delta reports the net live-count change of the overlay: staged adds
// minus staged removes.
func (o *exactOverlay) delta() int64 {
	if o == nil {
		return 0
	}
	return int64(len(o.adds)) - int64(len(o.removes))
}

// overlayGroup returns the contiguous run of entries with hash in s,
// which must be sorted by (Hash, ImageID). It returns a subslice and
// allocates nothing.
func overlayGroup(s []ExactEntry, hash Hash) []ExactEntry {
	lo, _ := slices.BinarySearchFunc(s, hash, func(e ExactEntry, h Hash) int {
		return compareHash(e.Hash, h)
	})
	hi := lo
	for hi < len(s) && s[hi].Hash == hash {
		hi++
	}
	return s[lo:hi]
}

// overlayContainsID reports whether id is present in group, which must
// hold entries of one hash in ascending ImageID order.
func overlayContainsID(group []ExactEntry, id ImageID) bool {
	_, found := slices.BinarySearchFunc(group, id, func(e ExactEntry, id ImageID) int {
		switch {
		case e.ImageID < id:
			return -1
		case e.ImageID > id:
			return 1
		default:
			return 0
		}
	})
	return found
}

// overlayContainsPair reports whether the exact pair is present in the
// sorted unique set s.
func overlayContainsPair(s []ExactEntry, e ExactEntry) bool {
	_, found := slices.BinarySearchFunc(s, e, compareEntry)
	return found
}

// subtractSortedEntries returns a minus b, both sorted unique. It
// allocates exactly the output.
func subtractSortedEntries(a, b []ExactEntry) []ExactEntry {
	if len(a) == 0 {
		return nil
	}
	if len(b) == 0 {
		return slices.Clone(a)
	}
	out := make([]ExactEntry, 0, len(a))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		c := compareEntry(a[i], b[j])
		switch {
		case c < 0:
			out = append(out, a[i])
			i++
		case c > 0:
			j++
		default:
			i++
			j++
		}
	}
	return append(out, a[i:]...)
}

// intersectSortedEntries returns the sorted unique intersection of two
// sorted unique sets. It allocates exactly the output.
func intersectSortedEntries(a, b []ExactEntry) []ExactEntry {
	var out []ExactEntry
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		c := compareEntry(a[i], b[j])
		switch {
		case c < 0:
			i++
		case c > 0:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// unionSortedEntries returns the sorted union of two sorted unique sets
// that are also disjoint. It allocates exactly the output.
func unionSortedEntries(a, b []ExactEntry) []ExactEntry {
	if len(a) == 0 {
		return slices.Clone(b)
	}
	if len(b) == 0 {
		return slices.Clone(a)
	}
	out := make([]ExactEntry, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if compareEntry(a[i], b[j]) < 0 {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

// stageOverlay folds a canonical batch (adds, removes) against the
// effective view into a replacement overlay over base blocks. Canonical
// here means what canonicalizeMutation guarantees: adds absent from the
// effective view, removes present in it.
//
// Across time the new batch wins: a pair staged by an earlier batch and
// renamed by this one leaves the overlay entirely, because its net state
// matches base either way (a re-added staged removal is base-present;
// a re-removed staged addition was never in base). Survivors merge
// linearly; no sorting, no maps, no block probes. It returns nil when the
// net staged set is empty.
func stageOverlay(cur *exactOverlay, adds, removes []ExactEntry) *exactOverlay {
	var cAdds, cRemoves []ExactEntry
	if cur != nil {
		cAdds, cRemoves = cur.adds, cur.removes
	}
	// New adds cancel staged removes; survivors join staged adds. The
	// survivors are absent from base: a surviving add is absent from the
	// effective view, and absent-from-effective plus absent-from-staged
	// (it would have cancelled instead) implies absent from base, since
	// base minus effective is exactly the staged removes.
	reAdded := intersectSortedEntries(cRemoves, adds)
	outRemoves := subtractSortedEntries(cRemoves, reAdded)
	freshAdds := subtractSortedEntries(adds, reAdded)
	// New removes cancel staged adds; survivors join staged removes by
	// the mirror argument.
	reRemoved := intersectSortedEntries(cAdds, removes)
	outAdds := subtractSortedEntries(cAdds, reRemoved)
	freshRemoves := subtractSortedEntries(removes, reRemoved)
	outAdds = unionSortedEntries(outAdds, freshAdds)
	outRemoves = unionSortedEntries(outRemoves, freshRemoves)
	if len(outAdds) == 0 && len(outRemoves) == 0 {
		return nil
	}
	return &exactOverlay{adds: outAdds, removes: outRemoves}
}

// baseOnlyView returns a view over the same blocks with no overlay. Fold
// and checkpoint-file paths use it so the block machinery, which only
// understands base pairs, sees canonical inputs.
func baseOnlyView(view *exactIndexView) *exactIndexView {
	return &exactIndexView{
		generation: view.generation,
		liveCount:  view.liveCount,
		blocks:     view.blocks,
	}
}

// maxPooledExactEntries bounds pooled entry-decode buffers. Buffers that
// grew past it are dropped instead of returned, so one huge block cannot
// pin megabytes in the pool.
const maxPooledExactEntries = 65536

var exactEntrySlicePool = sync.Pool{New: func() any {
	return make([]ExactEntry, 0, 256)
}}

// acquireExactEntries returns a scratch entry slice of length n. The
// caller must release it; contents are valid only until release.
func acquireExactEntries(n int) []ExactEntry {
	s, _ := exactEntrySlicePool.Get().([]ExactEntry)
	if cap(s) < n {
		return make([]ExactEntry, n)
	}
	return s[:n]
}

// releaseExactEntries returns scratch to the pool, dropping buffers that
// grew past maxPooledExactEntries. ExactEntry holds no pointers, so no
// clearing is required. Retained slices must never be released.
func releaseExactEntries(s []ExactEntry) {
	if cap(s) > maxPooledExactEntries {
		return
	}
	exactEntrySlicePool.Put(s[:0])
}
