package geoindex

import "github.com/evanoberholster/imageIdx/internal/indexutil"

// SearchRanges appends candidates from sorted, normalized key ranges. Covers
// produced by CoverRegion and RangesFromCellIDs satisfy this contract. Search
// allocates only if dst lacks result capacity.
func (g *Index) SearchRanges(dst []ImageID, ranges []Range) []ImageID {
	token := g.readers.Acquire()
	if !g.closed.Load() {
		view := g.view.Load()
		for rangeIndex := range ranges {
			keyRange := ranges[rangeIndex : rangeIndex+1]
			if keyRange[0].End < keyRange[0].Start {
				continue
			}
			first := shardForKey(keyRange[0].Start)
			if first >= geoShardCount {
				continue
			}
			last := shardForKey(keyRange[0].End)
			if last >= geoShardCount {
				last = geoShardCount - 1
			}
			for shardIndex := first; shardIndex <= last; shardIndex++ {
				shard := view.shards[shardIndex]
				if shard == nil {
					continue
				}
				dst = shard.sparse.searchRanges(dst, keyRange)
				dst = shard.dense.searchRanges(dst, keyRange)
			}
		}
	}
	g.readers.Release(token)
	return dst
}

// EstimateRanges returns the current candidate count for normalized ranges.
func (g *Index) EstimateRanges(ranges []Range) uint64 {
	token := g.readers.Acquire()
	defer g.readers.Release(token)
	if g.closed.Load() {
		return 0
	}
	view := g.view.Load()
	var estimate uint64
	for rangeIndex := range ranges {
		keyRange := ranges[rangeIndex : rangeIndex+1]
		if keyRange[0].End < keyRange[0].Start {
			continue
		}
		first := shardForKey(keyRange[0].Start)
		if first >= geoShardCount {
			continue
		}
		last := shardForKey(keyRange[0].End)
		if last >= geoShardCount {
			last = geoShardCount - 1
		}
		for shardIndex := first; shardIndex <= last; shardIndex++ {
			shard := view.shards[shardIndex]
			if shard == nil {
				continue
			}
			estimate = indexutil.SaturatingAdd(estimate, shard.sparse.estimateRanges(keyRange))
			estimate = indexutil.SaturatingAdd(estimate, shard.dense.estimateRanges(keyRange))
		}
	}
	return estimate
}

// AppendTo appends every indexed image ID. It is intended for diagnostics and
// the current full-scan Nearest implementation.
func (g *Index) AppendTo(dst []ImageID) []ImageID {
	token := g.readers.Acquire()
	if !g.closed.Load() {
		view := g.view.Load()
		for _, shard := range view.shards {
			if shard == nil {
				continue
			}
			dst = shard.sparse.searchRanges(dst, []Range{{Start: 0, End: ^S2Key(0)}})
			dst = shard.dense.searchRanges(dst, []Range{{Start: 0, End: ^S2Key(0)}})
		}
	}
	g.readers.Release(token)
	return dst
}
