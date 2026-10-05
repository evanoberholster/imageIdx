package simhash

import (
	"math/bits"
	"unsafe"

	hashkernel "github.com/evanoberholster/imageIdx/index/simhash/internal/hashkernel"
)

// SearchPDQBulk appends every committed record whose 256-bit PDQ hash is within
// maxDistance to dst, using the internal block kernel (NEON on arm64).
//
// It reads canonical columns with wide, plain loads, so it is NOT safe to run
// concurrently with a writer (Append/AppendBatch/Upsert/UpsertBatch). Use
// SearchPDQ for the concurrent-safe scalar path. Bulk is intended for quiescent
// scans, for example after ingest or on a read-only replica.
func (st *Store) SearchPDQBulk(dst []PDQMatch, query PDQHash, maxDistance uint16) []PDQMatch {
	if st.closed.Load() {
		return dst
	}
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	if st.closed.Load() {
		return dst
	}
	v := st.view.Load()
	last := clampCommitted(st.committedAtomic.Load(), v.capacity)
	if last == 0 {
		return dst
	}
	limit := int(maxDistance)
	qp := (*[32]byte)(unsafe.Pointer(&query))
	lastPage := hashPageOf(last)
	lastSlot := slotOf(last)
	for hashPage := uint64(0); hashPage <= lastPage; hashPage++ {
		sh, pi := v.shardForHashPage(hashPage)
		pageBase := sh.base(pi) + pdqColumnOffset
		maxSlot := slotsPerHashPage - 1
		if hashPage == lastPage {
			maxSlot = lastSlot
		}
		for s0 := 0; s0 <= maxSlot; s0 += hashkernel.BlockRecords {
			n := maxSlot - s0 + 1
			if n > hashkernel.BlockRecords {
				n = hashkernel.BlockRecords
			}
			off := pageBase + uint64(s0)*32
			mask := hashkernel.PDQBlockMask(qp, sh.arena[off:], n, limit)
			for mask != 0 {
				bit := bits.TrailingZeros64(mask)
				mask &= mask - 1
				slot := s0 + bit
				id := imageIDFromHashPage(hashPage, slot)
				if id == 0 {
					continue
				}
				d := hashkernel.PDQDistance(qp, (*[32]byte)(unsafe.Pointer(&sh.arena[pageBase+uint64(slot)*32])))
				dst = append(dst, PDQMatch{ImageID: id, Distance: d})
			}
		}
	}
	return dst
}
