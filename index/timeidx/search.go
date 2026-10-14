package timeidx

import (
	"sort"

	schema "github.com/evanoberholster/imageIdx/schema"
)

type ExactTimestampFunc func(schema.ValueID) (uint64, bool)

// rangeIndexes returns the half-open row span [lo, hi) whose timestamps fall in
// [start, end). Both bounds skip their binary search when the shard cannot
// contribute, which matters for shards fully inside a wide range.
func (s *shard) rangeIndexes(start, end uint64) (int, int) {
	lo, hi := 0, len(s.times)
	if s.times[0] < start {
		lo = sort.Search(hi, func(n int) bool { return s.times[n] >= start })
	}
	if s.times[hi-1] >= end {
		hi = sort.Search(hi, func(n int) bool { return s.times[n] >= end })
	}
	return lo, hi
}

// firstShardAtOrAfter returns the index of the first shard whose last time is
// at least start. Shards are ordered, so later shards cannot contain a match
// when this returns len(shards).
func firstShardAtOrAfter(shards []*shard, start uint64) int {
	return sort.Search(len(shards), func(n int) bool {
		s := shards[n]
		return s.times[len(s.times)-1] >= start
	})
}

// Search appends exact [start,end) matches in (TimeKey, ValueID) order. All
// shards come from one pinned snapshot. With sufficient dst capacity it does
// not allocate. start>=end is empty; MaxUint64 as end excludes that key.
func (i *Index) Search(dst []schema.ValueID, start, end uint64) []schema.ValueID {
	return i.search(dst, start, end, nil)
}

// SearchExact preserves the optional canonical-data filter for existing callers.
// It is not needed to correct quantization in the new format. The caller owns
// lookup consistency; it is not part of this index's snapshot transaction.
func (i *Index) SearchExact(dst []schema.ValueID, start, end uint64, lookup ExactTimestampFunc) []schema.ValueID {
	return i.search(dst, start, end, lookup)
}

func (i *Index) search(dst []schema.ValueID, start, end uint64, lookup ExactTimestampFunc) []schema.ValueID {
	if start >= end {
		return dst
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	if i.isClosed() {
		return dst
	}
	shards := i.view.Load().shards
	for _, s := range shards[firstShardAtOrAfter(shards, start):] {
		if s.times[0] >= end {
			break
		}
		lo, hi := s.rangeIndexes(start, end)
		if lookup == nil {
			dst = append(dst, s.ids[lo:hi]...)
			continue
		}
		for _, id := range s.ids[lo:hi] {
			if t, ok := lookup(id); ok && t >= start && t < end {
				dst = append(dst, id)
			}
		}
	}
	return dst
}

func (i *Index) EstimateRange(start, end uint64) uint64 {
	if start >= end {
		return 0
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	if i.isClosed() {
		return 0
	}
	shards := i.view.Load().shards
	var count uint64
	for _, s := range shards[firstShardAtOrAfter(shards, start):] {
		if s.times[0] >= end {
			break
		}
		lo, hi := s.rangeIndexes(start, end)
		count += uint64(hi - lo)
	}
	return count
}
