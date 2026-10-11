package timeidx

import (
	"sort"

	"github.com/evanoberholster/imageIdx/common"
)

type ExactTimestampFunc func(common.ValueID) (uint64, bool)

// Search appends exact [start,end) matches in (TimeKey, ValueID) order. All
// shards come from one pinned snapshot. With sufficient dst capacity it does
// not allocate. start>=end is empty; MaxUint64 as end excludes that key.
func (i *Index) Search(dst []common.ValueID, start, end uint64) []common.ValueID {
	return i.search(dst, start, end, nil)
}

// SearchExact preserves the optional canonical-data filter for existing callers.
// It is not needed to correct quantization in the new format. The caller owns
// lookup consistency; it is not part of this index's snapshot transaction.
func (i *Index) SearchExact(dst []common.ValueID, start, end uint64, lookup ExactTimestampFunc) []common.ValueID {
	return i.search(dst, start, end, lookup)
}

func (i *Index) search(dst []common.ValueID, start, end uint64, lookup ExactTimestampFunc) []common.ValueID {
	if start >= end {
		return dst
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	if i.closed.Load() {
		return dst
	}
	shards := i.view.Load().shards
	first := sort.Search(len(shards), func(n int) bool { s := shards[n]; return s.times[len(s.times)-1] >= start })
	for _, s := range shards[first:] {
		if s.times[0] >= end {
			break
		}
		lo := sort.Search(len(s.times), func(n int) bool { return s.times[n] >= start })
		hi := sort.Search(len(s.times), func(n int) bool { return s.times[n] >= end })
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
	if i.closed.Load() {
		return 0
	}
	shards := i.view.Load().shards
	first := sort.Search(len(shards), func(n int) bool { s := shards[n]; return s.times[len(s.times)-1] >= start })
	var count uint64
	for _, s := range shards[first:] {
		if s.times[0] >= end {
			break
		}
		lo := sort.Search(len(s.times), func(n int) bool { return s.times[n] >= start })
		hi := sort.Search(len(s.times), func(n int) bool { return s.times[n] >= end })
		count += uint64(hi - lo)
	}
	return count
}
