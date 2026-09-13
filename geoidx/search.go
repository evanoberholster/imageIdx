package geoidx

import (
	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/roar"
)

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

// CollectRanges accumulates the candidates of sorted, normalized key ranges
// into the transient MutableBitmap m, which the caller resets first. Dense
// cells fold without decoding to IDs; sparse slots stage raw IDs. Materialize
// once with m.AppendTo for sorted, unique output. It allocates only as the
// accumulation grows.
func (g *Index) CollectRanges(m *roar.MutableBitmap, ranges []Range) {
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
				shard.sparse.collectRanges(m, keyRange)
				shard.dense.collectRanges(m, keyRange)
			}
		}
	}
	g.readers.Release(token)
}

// FilterRanges appends the candidates present in any of ranges to dst.
// It verifies candidates against the covered cells instead of materializing
// every ID in the ranges, so it wins when candidates are few relative to the
// covered population. Each image lives in exactly one cell and ranges are
// disjoint, so matches carry no duplicates; however matches arrive grouped by
// cell rather than globally sorted, and the caller must re-sort when order
// matters. It allocates only if dst lacks result capacity.
func (g *Index) FilterRanges(dst, candidates []ImageID, ranges []Range) []ImageID {
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
				dst = shard.sparse.filterRanges(dst, candidates, keyRange)
				dst = shard.dense.filterRanges(dst, candidates, keyRange)
			}
		}
	}
	g.readers.Release(token)
	return dst
}

// CountCells returns the number of indexed cells covered by normalized
// ranges. It walks only key tables, never ID data.
func (g *Index) CountCells(ranges []Range) int {
	token := g.readers.Acquire()
	count := 0
	if !g.closed.Load() {
		view := g.view.Load()
		for rangeIndex := range ranges {
			keyRange := ranges[rangeIndex]
			if keyRange.End < keyRange.Start {
				continue
			}
			first := shardForKey(keyRange.Start)
			if first >= geoShardCount {
				continue
			}
			last := shardForKey(keyRange.End)
			if last >= geoShardCount {
				last = geoShardCount - 1
			}
			for shardIndex := first; shardIndex <= last; shardIndex++ {
				shard := view.shards[shardIndex]
				if shard == nil {
					continue
				}
				count += countCells(shard.sparse.keys, keyRange) + countCells(shard.dense.keys, keyRange)
			}
		}
	}
	g.readers.Release(token)
	return count
}

func countCells(keys []S2Key, keyRange Range) int {
	start := lowerBound(keys, keyRange.Start)
	end := upperBound(keys, keyRange.End)
	return end - start
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
