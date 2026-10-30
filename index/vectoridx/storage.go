package vectoridx

import (
	"sort"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/roar"
)

// cloneSnapshot shallow-copies slice headers sharing backing arrays.
// Appends beyond the published length are safe in place; middle inserts copy.
func cloneSnapshot(s *vectorSnapshot) *vectorSnapshot {
	c := *s
	return &c
}

// Vector chunks bound append growth and copy-on-write update cost. Sixty-four
// vectors is 128-256 KiB at the supported 512-1024 dimensions.
const vectorChunkShift = 6

const vectorChunkVectors = 1 << vectorChunkShift

func (s *vectorSnapshot) vectorChunkCount() int {
	return (len(s.ids) + vectorChunkVectors - 1) >> vectorChunkShift
}

func (s *vectorSnapshot) newVectorChunk() []byte {
	return make([]byte, vectorChunkVectors*s.vecBytes)
}

// appendVectorAt writes vector ord into the chunk directory. ord must equal
// the current published count; existing snapshots are unaffected because they
// bound reads by their older len(ids).
func (s *vectorSnapshot) appendVectorAt(ord int, raw []byte) {
	ci := ord >> vectorChunkShift
	if ci == len(s.vectors) {
		s.vectors = append(s.vectors, s.newVectorChunk())
	}
	off := (ord & (vectorChunkVectors - 1)) * s.vecBytes
	copy(s.vectors[ci][off:off+s.vecBytes], raw)
}

// replaceVectorAt replaces a published vector with copy-on-write at chunk
// granularity. Readers holding an older snapshot keep the old chunk.
func (s *vectorSnapshot) replaceVectorAt(ord int, raw []byte) {
	ci := ord >> vectorChunkShift
	if ci >= len(s.vectors) {
		s.appendVectorAt(ord, raw)
		return
	}
	old := s.vectors[ci]
	chunk := make([]byte, len(old))
	copy(chunk, old)
	off := (ord & (vectorChunkVectors - 1)) * s.vecBytes
	copy(chunk[off:off+s.vecBytes], raw)
	// The directory slice is shared with published snapshots; clone it before
	// swapping one chunk pointer.
	dir := make([][]byte, len(s.vectors))
	copy(dir, s.vectors)
	dir[ci] = chunk
	s.vectors = dir
}

// vectorChunks returns only the published portion of each chunk, suitable for
// streaming to a base generation without materializing a contiguous copy.
func (s *vectorSnapshot) vectorChunks() [][]byte {
	n := s.vectorChunkCount()
	if n == 0 {
		return nil
	}
	chunks := make([][]byte, n)
	for i := range chunks {
		count := min(vectorChunkVectors, len(s.ids)-i*vectorChunkVectors)
		chunks[i] = s.vectors[i][:count*s.vecBytes]
	}
	return chunks
}

// pickNeighbors selects up to m candidates using the configured policy.
func pickNeighbors(s *vectorSnapshot, cands []searchEntry, m int) []searchEntry {
	if s.cfg.UseHeuristic {
		return selectHeuristic(cands, m, s)
	}
	return selectClosest(cands, m)
}

// sortedInsert inserts id->ord into the snapshot's sorted locator. Appends at
// the end reuse backing safely; middle inserts allocate new arrays so
// published readers never see a shifted range.
func sortedInsert(s *vectorSnapshot, id NodeID, ord int) {
	if s.useDense {
		return
	}
	pos := sort.Search(len(s.sortedIDs), func(i int) bool { return s.sortedIDs[i] >= id })
	if pos == len(s.sortedIDs) {
		s.sortedIDs = append(s.sortedIDs, id)
		s.sortedOrd = append(s.sortedOrd, ord)
		return
	}
	newIDs := make([]NodeID, len(s.sortedIDs)+1)
	newOrd := make([]int, len(s.sortedOrd)+1)
	copy(newIDs, s.sortedIDs[:pos])
	copy(newOrd, s.sortedOrd[:pos])
	newIDs[pos] = id
	newOrd[pos] = ord
	copy(newIDs[pos+1:], s.sortedIDs[pos:])
	copy(newOrd[pos+1:], s.sortedOrd[pos:])
	s.sortedIDs = newIDs
	s.sortedOrd = newOrd
}

const maxDenseEntries = 1 << 26

// denseFeasible reports whether [min,max] fits a dense table at count nodes.
func denseFeasible(min, max NodeID, count int) bool {
	if max < min {
		return true
	}
	rng := uint64(max-min) + 1
	if rng > maxDenseEntries {
		return false
	}
	if rng <= uint64(count)*4+1024 {
		return true
	}
	return false
}

// denseEnsure makes the dense table cover id when feasible, building it from
// the snapshot on first use. It returns false when the index stays sparse.
func denseEnsure(s *vectorSnapshot, id NodeID, count int) bool {
	if s.useDense && s.dense != nil {
		min, mx := s.dense.min, s.dense.min+NodeID(len(s.dense.vals))-1
		lo, hi := min, mx
		if id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
		if !denseFeasible(lo, hi, count) {
			s.dense = nil
			s.useDense = false
			s.rebuildSorted()
			// rebuildSorted already included id: report the locator as
			// updated so the caller does not insert it a second time.
			return true
		}
		if id < s.dense.min {
			gap := uint64(s.dense.min-id) + uint64(len(s.dense.vals))
			_ = gap
			// Prepend requires rebase: rebuild (rare).
			s.rebuildDenseWith(lo, hi)
			return true
		}
		if want := uint64(id-s.dense.min) + 1; want > uint64(len(s.dense.vals)) {
			need := int(want) - len(s.dense.vals)
			s.dense.vals = append(s.dense.vals, make([]int32, need)...)
			for i := len(s.dense.vals) - need; i < len(s.dense.vals); i++ {
				s.dense.vals[i] = -1
			}
		}
		return true
	}
	// Sparse: check feasibility including id.
	var lo, hi NodeID
	if len(s.ids) == 0 {
		lo, hi = id, id
	} else {
		lo, hi = s.ids[0], s.ids[0]
		for _, x := range s.ids {
			if x < lo {
				lo = x
			}
			if x > hi {
				hi = x
			}
		}
		if id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
	}
	_ = hi
	if !denseFeasible(lo, hi, count) {
		return false
	}
	s.rebuildDenseWith(lo, hi)
	return true
}

// rebuildDenseWith builds a fresh dense table over [lo,hi] from current ids.
func (s *vectorSnapshot) rebuildDenseWith(lo, hi NodeID) {
	n := int(uint64(hi-lo) + 1)
	vals := make([]int32, n)
	for i := range vals {
		vals[i] = -1
	}
	for ord, id := range s.ids {
		if id >= lo && id <= hi {
			vals[uint64(id-lo)] = int32(ord)
		}
	}
	s.dense = &denseTable{min: lo, vals: vals}
	s.useDense = true
	s.sortedIDs = nil
	s.sortedOrd = nil
}

// rebuildSorted rebuilds the sorted locator from ids (sparse fallback).
func (s *vectorSnapshot) rebuildSorted() {
	n := len(s.ids)
	s.sortedIDs = make([]NodeID, n)
	s.sortedOrd = make([]int, n)
	for ord, id := range s.ids {
		s.sortedIDs[ord] = id
		s.sortedOrd[ord] = ord
	}
	sort.Slice(s.sortedIDs, func(i, j int) bool { return s.sortedIDs[i] < s.sortedIDs[j] })
	pos := make(map[NodeID]int, n)
	for ord, id := range s.ids {
		pos[id] = ord
	}
	for i, id := range s.sortedIDs {
		s.sortedOrd[i] = pos[id]
	}
}

// denseSet records id->ord with atomic stores safe for lock-free readers.
func denseSet(s *vectorSnapshot, id NodeID, ord int) {
	dt := s.dense
	if dt == nil || id < dt.min {
		return
	}
	idx := uint64(id - dt.min)
	if idx >= uint64(len(dt.vals)) {
		return
	}
	atomic.StoreInt32(&dt.vals[idx], int32(ord))
}

// rebuildTombeset publishes a new immutable tomb bitmap from a sorted list.
func rebuildTombeset(sorted []NodeID) (roar.Bitmap, []NodeID) {
	cp := append([]NodeID(nil), sorted...)
	if len(cp) == 0 {
		return roar.Bitmap{}, cp
	}
	u64 := make([]uint64, len(cp))
	for i, id := range cp {
		u64[i] = uint64(id)
	}
	bm, err := roar.FromSorted(u64)
	if err != nil {
		return roar.Bitmap{}, cp
	}
	return bm, cp
}

// insertTombSorted returns sorted with id inserted (copy).
func insertTombSorted(sorted []NodeID, id NodeID) []NodeID {
	pos := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= id })
	if pos < len(sorted) && sorted[pos] == id {
		return sorted
	}
	out := make([]NodeID, len(sorted)+1)
	copy(out, sorted[:pos])
	out[pos] = id
	copy(out[pos+1:], sorted[pos:])
	return out
}

// removeTombSorted returns sorted without id (copy).
func removeTombSorted(sorted []NodeID, id NodeID) []NodeID {
	pos := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= id })
	if pos >= len(sorted) || sorted[pos] != id {
		return sorted
	}
	out := make([]NodeID, len(sorted)-1)
	copy(out, sorted[:pos])
	copy(out[pos:], sorted[pos+1:])
	return out
}

func (v *VectorIdx) addRawLocked(cur *vectorSnapshot, id NodeID, raw []byte) (*vectorSnapshot, error) {
	return v.addRawLockedAtLevel(cur, id, raw, -1)
}
