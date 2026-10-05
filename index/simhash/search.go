package simhash

import (
	"math/bits"
	"runtime"
	"sync/atomic"
	"unsafe"

	common "github.com/evanoberholster/imageIdx/common"
)

// SIMD note: the scalar scans below use sync/atomic loads so they are correct
// against concurrent in-place point updates. A future AVX2/AVX-512/NEON kernel
// cannot use plain wide loads for the same guarantee, because a 32-byte PDQ
// store is not atomic. Before enabling SIMD, one of the following must be
// proven and benchmarked: (a) SIMD reads are combined with the block sequence
// validation and shown safe with a concurrent writer, (b) SIMD scans run in a
// quiescent/update epoch with no concurrent writer, or (c) the scanned memory
// is immutable (append-only). The scalar concurrent-safe path is the reference
// implementation and must not be replaced merely for benchmark numbers.

// seqSpinLimit bounds a reader's tight retry before it yields.
const seqSpinLimit = 64

// waitSeqEven returns the current sequence once it is even, spinning briefly
// and yielding only if the writer stays in its critical section.
func waitSeqEven(seq *uint64) uint64 {
	for spin := 0; ; spin++ {
		v := atomic.LoadUint64(seq)
		if v&1 == 0 {
			return v
		}
		if spin >= seqSpinLimit {
			runtime.Gosched()
		}
	}
}

// clampCommitted limits a reader's committed high-water mark to what its
// snapshot's shard set can address, so a reader that sees a newer committed mark
// than its view scans a consistent older snapshot instead of a missing shard.
func clampCommitted(committed, capacity uint64) uint64 {
	if committed > capacity {
		return capacity
	}
	return committed
}

// In the middle of the PDQ distance range the first-word reject branch is close
// to 50/50 and mispredicts badly, so a two-word prefix (still branchy but
// predictable) is faster. Early rejection wins when the threshold is far below
// or far above the per-word mean.
const (
	pdqEarlyRejectMax = 20
	pdqEarlyRejectMin = 40
)

func useEarlyReject(limit int) bool {
	return limit <= pdqEarlyRejectMax || limit >= pdqEarlyRejectMin
}

// loadU64 is an atomic 64-bit load at a byte offset in the arena. The caller
// guarantees 8-byte alignment (all column offsets are 64-byte aligned).
func loadU64(arena []byte, off uint64) uint64 {
	return atomic.LoadUint64((*uint64)(unsafe.Pointer(&arena[off])))
}

// SearchPHash appends every committed record whose 64-bit pHash is within
// maxDistance to dst, with the exact distance.
func (st *Store) SearchPHash(dst []PHashMatch, query uint64, maxDistance int) []PHashMatch {
	if st.closed.Load() {
		return dst
	}
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	if st.closed.Load() {
		return dst
	}
	v := st.view.Load()
	return v.searchPHash(dst, query, maxDistance, clampCommitted(st.committedAtomic.Load(), v.capacity))
}

// SearchPDQ appends every committed record whose 256-bit PDQ hash is within
// maxDistance to dst, with the exact distance. PDQ is evaluated independently
// of pHash.
func (st *Store) SearchPDQ(dst []PDQMatch, query PDQHash, maxDistance uint16) []PDQMatch {
	if st.closed.Load() {
		return dst
	}
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	if st.closed.Load() {
		return dst
	}
	v := st.view.Load()
	return v.searchPDQ(dst, &query, maxDistance, clampCommitted(st.committedAtomic.Load(), v.capacity))
}

// SearchCombined appends every committed record that satisfies both the pHash
// and PDQ thresholds, with both exact distances. order selects which column is
// evaluated first; the AND semantics are identical.
func (st *Store) SearchCombined(dst []CombinedMatch, phashQuery uint64, maxPHashDistance int, pdqQuery PDQHash, maxPDQDistance uint16, order Order) []CombinedMatch {
	if st.closed.Load() {
		return dst
	}
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	if st.closed.Load() {
		return dst
	}
	v := st.view.Load()
	return v.searchCombined(dst, phashQuery, maxPHashDistance, &pdqQuery, maxPDQDistance, order, clampCommitted(st.committedAtomic.Load(), v.capacity))
}

func (v *view) searchPHash(dst []PHashMatch, query uint64, maxDistance int, last uint64) []PHashMatch {
	if last == 0 {
		return dst
	}
	lastPage := hashPageOf(last)
	lastSlot := slotOf(last)
	blockRecords := v.geo.seqBlockRecords
	for hashPage := uint64(0); hashPage <= lastPage; hashPage++ {
		sh, pi := v.shardForHashPage(hashPage)
		pageIDBase := hashPage << 16
		pHashBase := sh.base(pi)
		maxSlot := slotsPerHashPage - 1
		if hashPage == lastPage {
			maxSlot = lastSlot
		}
		for s0 := 0; s0 <= maxSlot; s0 += blockRecords {
			s1 := s0 + blockRecords - 1
			if s1 > maxSlot {
				s1 = maxSlot
			}
			seq := sh.seqPtr(pi, s0/blockRecords)
			mark := len(dst)
			for {
				a := waitSeqEven(seq)
				for slot := s0; slot <= s1; slot++ {
					id := pageIDBase | uint64(slot)
					if id == 0 {
						continue
					}
					d := bits.OnesCount64(query ^ loadU64(sh.arena, pHashBase+uint64(slot)*8))
					if d <= maxDistance {
						dst = append(dst, PHashMatch{ImageID: id, Distance: d})
					}
				}
				if atomic.LoadUint64(seq) == a {
					break
				}
				dst = dst[:mark]
			}
		}
	}
	return dst
}

func (v *view) searchPDQ(dst []PDQMatch, query *PDQHash, maxDistance uint16, last uint64) []PDQMatch {
	if last == 0 {
		return dst
	}
	lastPage := hashPageOf(last)
	lastSlot := slotOf(last)
	blockRecords := v.geo.seqBlockRecords
	limit := int(maxDistance)
	early := useEarlyReject(limit)
	qw := hashWords(query)
	for hashPage := uint64(0); hashPage <= lastPage; hashPage++ {
		sh, pi := v.shardForHashPage(hashPage)
		pageIDBase := hashPage << 16
		pdqBase := sh.base(pi) + pdqColumnOffset
		maxSlot := slotsPerHashPage - 1
		if hashPage == lastPage {
			maxSlot = lastSlot
		}
		for s0 := 0; s0 <= maxSlot; s0 += blockRecords {
			s1 := s0 + blockRecords - 1
			if s1 > maxSlot {
				s1 = maxSlot
			}
			seq := sh.seqPtr(pi, s0/blockRecords)
			mark := len(dst)
			for {
				a := waitSeqEven(seq)
				for slot := s0; slot <= s1; slot++ {
					id := pageIDBase | uint64(slot)
					if id == 0 {
						continue
					}
					rec := pdqBase + uint64(slot)*32
					if early {
						d := bits.OnesCount64(qw[0] ^ loadU64(sh.arena, rec))
						if d > limit {
							continue
						}
						d += bits.OnesCount64(qw[1] ^ loadU64(sh.arena, rec+8))
						if d > limit {
							continue
						}
						d += bits.OnesCount64(qw[2] ^ loadU64(sh.arena, rec+16))
						if d > limit {
							continue
						}
						d += bits.OnesCount64(qw[3] ^ loadU64(sh.arena, rec+24))
						if d > limit {
							continue
						}
						dst = append(dst, PDQMatch{ImageID: id, Distance: uint16(d)})
						continue
					}
					d := bits.OnesCount64(qw[0]^loadU64(sh.arena, rec)) +
						bits.OnesCount64(qw[1]^loadU64(sh.arena, rec+8))
					if d <= limit {
						d += bits.OnesCount64(qw[2]^loadU64(sh.arena, rec+16)) +
							bits.OnesCount64(qw[3]^loadU64(sh.arena, rec+24))
						if d <= limit {
							dst = append(dst, PDQMatch{ImageID: id, Distance: uint16(d)})
						}
					}
				}
				if atomic.LoadUint64(seq) == a {
					break
				}
				dst = dst[:mark]
			}
		}
	}
	return dst
}

func (v *view) searchCombined(dst []CombinedMatch, phashQuery uint64, maxPHash int, pdqQuery *PDQHash, maxPDQ uint16, order Order, last uint64) []CombinedMatch {
	if last == 0 {
		return dst
	}
	lastPage := hashPageOf(last)
	lastSlot := slotOf(last)
	blockRecords := v.geo.seqBlockRecords
	limit := int(maxPDQ)
	early := useEarlyReject(limit)
	qw := hashWords(pdqQuery)
	for hashPage := uint64(0); hashPage <= lastPage; hashPage++ {
		sh, pi := v.shardForHashPage(hashPage)
		pageIDBase := hashPage << 16
		base := sh.base(pi)
		pHashBase := base
		pdqBase := base + pdqColumnOffset
		maxSlot := slotsPerHashPage - 1
		if hashPage == lastPage {
			maxSlot = lastSlot
		}
		for s0 := 0; s0 <= maxSlot; s0 += blockRecords {
			s1 := s0 + blockRecords - 1
			if s1 > maxSlot {
				s1 = maxSlot
			}
			seq := sh.seqPtr(pi, s0/blockRecords)
			mark := len(dst)
			for {
				a := waitSeqEven(seq)
				for slot := s0; slot <= s1; slot++ {
					id := pageIDBase | uint64(slot)
					if id == 0 {
						continue
					}
					if order == PHashFirst {
						ph := bits.OnesCount64(phashQuery ^ loadU64(sh.arena, pHashBase+uint64(slot)*8))
						if ph > maxPHash {
							continue
						}
						d, ok := pdqWithin(sh.arena, pdqBase, slot, qw, limit, early)
						if !ok {
							continue
						}
						dst = append(dst, CombinedMatch{ImageID: id, PHashDistance: ph, PDQDistance: d})
						continue
					}
					d, ok := pdqWithin(sh.arena, pdqBase, slot, qw, limit, early)
					if !ok {
						continue
					}
					ph := bits.OnesCount64(phashQuery ^ loadU64(sh.arena, pHashBase+uint64(slot)*8))
					if ph > maxPHash {
						continue
					}
					dst = append(dst, CombinedMatch{ImageID: id, PHashDistance: ph, PDQDistance: d})
				}
				if atomic.LoadUint64(seq) == a {
					break
				}
				dst = dst[:mark]
			}
		}
	}
	return dst
}

// pdqWithin computes the exact PDQ distance for one record whose four words
// start at byte offset pdqBase+slot*32. ok is false when the distance exceeds
// limit. It uses word-at-a-time early rejection for low/high thresholds and a
// two-word prefix for the mid range, matching the standalone scan.
func pdqWithin(arena []byte, pdqBase uint64, slot int, qw *[4]uint64, limit int, early bool) (uint16, bool) {
	rec := pdqBase + uint64(slot)*32
	if early {
		d := bits.OnesCount64(qw[0] ^ loadU64(arena, rec))
		if d > limit {
			return 0, false
		}
		d += bits.OnesCount64(qw[1] ^ loadU64(arena, rec+8))
		if d > limit {
			return 0, false
		}
		d += bits.OnesCount64(qw[2] ^ loadU64(arena, rec+16))
		if d > limit {
			return 0, false
		}
		d += bits.OnesCount64(qw[3] ^ loadU64(arena, rec+24))
		if d > limit {
			return 0, false
		}
		return uint16(d), true
	}
	d := bits.OnesCount64(qw[0]^loadU64(arena, rec)) +
		bits.OnesCount64(qw[1]^loadU64(arena, rec+8))
	if d > limit {
		return 0, false
	}
	d += bits.OnesCount64(qw[2]^loadU64(arena, rec+16)) +
		bits.OnesCount64(qw[3]^loadU64(arena, rec+24))
	return uint16(d), d <= limit
}

// Quality returns the PDQ quality byte for id, validated by the block sequence.
func (st *Store) Quality(id common.ImageID) (uint8, bool) {
	if st.closed.Load() {
		return 0, false
	}
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	if st.closed.Load() {
		return 0, false
	}
	v := st.view.Load()
	committed := clampCommitted(st.committedAtomic.Load(), v.capacity)
	if id == 0 || uint64(id) > committed {
		return 0, false
	}
	hashPage := hashPageOf(uint64(id))
	slot := slotOf(uint64(id))
	sh, pi := v.shardForHashPage(hashPage)
	seq := sh.seqPtr(pi, v.geo.blockOf(slot))
	src := (*uint64)(unsafe.Pointer(&sh.arena[sh.base(pi)+qualityColumnOffset+uint64(slot&^7)]))
	for {
		a := waitSeqEven(seq)
		q := uint8(atomic.LoadUint64(src) >> qualityShift(slot))
		if atomic.LoadUint64(seq) == a {
			return q, true
		}
	}
}
