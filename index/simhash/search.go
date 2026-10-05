package simhash

import (
	"math/bits"
	"runtime"
	"sync/atomic"

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

// seqSpinLimit bounds a reader's tight retry before it yields. The writer's
// critical section is a handful of atomic stores, so a short spin usually
// succeeds immediately.
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

// SearchPHash appends every committed record whose 64-bit pHash is within
// maxDistance to dst, with the exact distance.
func (st *Store) SearchPHash(dst []PHashMatch, query uint64, maxDistance int) []PHashMatch {
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	return st.view.Load().searchPHash(dst, query, maxDistance)
}

// SearchPDQ appends every committed record whose 256-bit PDQ hash is within
// maxDistance to dst, with the exact distance. PDQ is evaluated independently
// of pHash.
func (st *Store) SearchPDQ(dst []PDQMatch, query PDQHash, maxDistance uint16) []PDQMatch {
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	return st.view.Load().searchPDQ(dst, &query, maxDistance)
}

// SearchCombined appends every committed record that satisfies both the pHash
// and PDQ thresholds, with both exact distances. order selects which column is
// evaluated first; the AND semantics are identical.
func (st *Store) SearchCombined(dst []CombinedMatch, phashQuery uint64, maxPHashDistance int, pdqQuery PDQHash, maxPDQDistance uint16, order Order) []CombinedMatch {
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	return st.view.Load().searchCombined(dst, phashQuery, maxPHashDistance, &pdqQuery, maxPDQDistance, order)
}

func (v *view) searchPHash(dst []PHashMatch, query uint64, maxDistance int) []PHashMatch {
	last := v.committed
	if last == 0 {
		return dst
	}
	lastPage := hashPageOf(last)
	lastSlot := slotOf(last)
	blockRecords := v.geo.seqBlockRecords
	for hashPage := uint64(0); hashPage <= lastPage; hashPage++ {
		sh, pi := v.shardForHashPage(hashPage)
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
					id := imageIDFromHashPage(hashPage, slot)
					if id == 0 {
						continue
					}
					d := bits.OnesCount64(query ^ atomic.LoadUint64(sh.phash(pi, slot)))
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

func (v *view) searchPDQ(dst []PDQMatch, query *PDQHash, maxDistance uint16) []PDQMatch {
	last := v.committed
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
					id := imageIDFromHashPage(hashPage, slot)
					if id == 0 {
						continue
					}
					if early {
						d := bits.OnesCount64(qw[0] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 0)))
						if d > limit {
							continue
						}
						d += bits.OnesCount64(qw[1] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 1)))
						if d > limit {
							continue
						}
						d += bits.OnesCount64(qw[2] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 2)))
						if d > limit {
							continue
						}
						d += bits.OnesCount64(qw[3] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 3)))
						if d > limit {
							continue
						}
						dst = append(dst, PDQMatch{ImageID: id, Distance: uint16(d)})
						continue
					}
					// Two-word prefix: for mid-range thresholds the first
					// check almost always rejects, so the branch is
					// predictable and half the popcounts are skipped.
					d := bits.OnesCount64(qw[0]^atomic.LoadUint64(sh.pdqWord(pi, slot, 0))) +
						bits.OnesCount64(qw[1]^atomic.LoadUint64(sh.pdqWord(pi, slot, 1)))
					if d <= limit {
						d += bits.OnesCount64(qw[2]^atomic.LoadUint64(sh.pdqWord(pi, slot, 2))) +
							bits.OnesCount64(qw[3]^atomic.LoadUint64(sh.pdqWord(pi, slot, 3)))
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

func (v *view) searchCombined(dst []CombinedMatch, phashQuery uint64, maxPHash int, pdqQuery *PDQHash, maxPDQ uint16, order Order) []CombinedMatch {
	last := v.committed
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
					id := imageIDFromHashPage(hashPage, slot)
					if id == 0 {
						continue
					}
					if order == PHashFirst {
						ph := bits.OnesCount64(phashQuery ^ atomic.LoadUint64(sh.phash(pi, slot)))
						if ph > maxPHash {
							continue
						}
						d, ok := pdqWithin(sh, pi, slot, qw, limit, early)
						if !ok {
							continue
						}
						dst = append(dst, CombinedMatch{ImageID: id, PHashDistance: ph, PDQDistance: d})
						continue
					}
					d, ok := pdqWithin(sh, pi, slot, qw, limit, early)
					if !ok {
						continue
					}
					ph := bits.OnesCount64(phashQuery ^ atomic.LoadUint64(sh.phash(pi, slot)))
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

// pdqWithin computes the exact PDQ distance, using early rejection or the
// two-word-prefix path. ok is false when the distance exceeds limit.
func pdqWithin(sh *shard, pi uint64, slot int, qw *[4]uint64, limit int, early bool) (uint16, bool) {
	if early {
		return pdqDistanceAt(sh, pi, slot, qw, limit)
	}
	d := bits.OnesCount64(qw[0]^atomic.LoadUint64(sh.pdqWord(pi, slot, 0))) +
		bits.OnesCount64(qw[1]^atomic.LoadUint64(sh.pdqWord(pi, slot, 1)))
	if d > limit {
		return 0, false
	}
	d += bits.OnesCount64(qw[2]^atomic.LoadUint64(sh.pdqWord(pi, slot, 2))) +
		bits.OnesCount64(qw[3]^atomic.LoadUint64(sh.pdqWord(pi, slot, 3)))
	return uint16(d), d <= limit
}

// pdqDistanceAt computes the exact PDQ distance with early rejection. ok is
// false when the partial distance already exceeds limit.
func pdqDistanceAt(sh *shard, pi uint64, slot int, qw *[4]uint64, limit int) (uint16, bool) {
	d := bits.OnesCount64(qw[0] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 0)))
	if d > limit {
		return 0, false
	}
	d += bits.OnesCount64(qw[1] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 1)))
	if d > limit {
		return 0, false
	}
	d += bits.OnesCount64(qw[2] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 2)))
	if d > limit {
		return 0, false
	}
	d += bits.OnesCount64(qw[3] ^ atomic.LoadUint64(sh.pdqWord(pi, slot, 3)))
	if d > limit {
		return 0, false
	}
	return uint16(d), true
}

// Quality returns the PDQ quality byte for id, validated by the block sequence.
func (st *Store) Quality(id common.ImageID) (uint8, bool) {
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	v := st.view.Load()
	if id == 0 || uint64(id) > v.committed {
		return 0, false
	}
	hashPage := hashPageOf(uint64(id))
	slot := slotOf(uint64(id))
	sh, pi := v.shardForHashPage(hashPage)
	seq := sh.seqPtr(pi, slot/v.geo.seqBlockRecords)
	for {
		a := waitSeqEven(seq)
		q := uint8(atomic.LoadUint64(sh.qualityWord(pi, slot)) >> qualityShift(slot))
		if atomic.LoadUint64(seq) == a {
			return q, true
		}
	}
}
