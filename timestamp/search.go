package timestamp

import "github.com/evanoberholster/imageIdx/internal/indexutil"

type ExactTimestampFunc func(ImageID) (uint64, bool)

// Search appends compressed-index candidates for [start, end) to dst. The
// caller controls allocation by supplying enough spare capacity in dst.
func (i *Index) Search(dst []ImageID, start, end uint64) []ImageID {
	lo, hi, ok := markerRange(start, end, i.offset)
	if !ok {
		return dst
	}
	epoch := i.epoch.enter()
	if i.closed.Load() {
		i.epoch.exit(epoch)
		return dst
	}
	directory := i.directory.Load()
	first := directory.firstOverlapping(lo)
	for index := first; index < len(directory.entries); index++ {
		entry := &directory.entries[index]
		if entry.startMarker >= hi {
			break
		}
		view := entry.block.view.Load()
		dst = searchBlock(dst, view, lo, hi, 0, 0, nil)
	}
	i.epoch.exit(epoch)
	return dst
}

// SearchExact filters lossy compressed candidates through the canonical
// timestamp lookup. The lookup must be safe for concurrent reads.
func (i *Index) SearchExact(dst []ImageID, start, end uint64, lookup ExactTimestampFunc) []ImageID {
	if lookup == nil {
		return i.Search(dst, start, end)
	}
	lo, hi, ok := markerRange(start, end, i.offset)
	if !ok {
		return dst
	}
	epoch := i.epoch.enter()
	if i.closed.Load() {
		i.epoch.exit(epoch)
		return dst
	}
	directory := i.directory.Load()
	first := directory.firstOverlapping(lo)
	for index := first; index < len(directory.entries); index++ {
		entry := &directory.entries[index]
		if entry.startMarker >= hi {
			break
		}
		view := entry.block.view.Load()
		dst = searchBlock(dst, view, lo, hi, start, end, lookup)
	}
	i.epoch.exit(epoch)
	return dst
}

// EstimateRange returns an upper bound for compressed-index candidates in
// [start, end). It inspects marker counts but does not materialize ImageIDs,
// making it suitable for cross-index query planning. Delta deletes and repeated
// updates may make the estimate larger than the eventual result.
func (i *Index) EstimateRange(start, end uint64) uint64 {
	lo, hi, ok := markerRange(start, end, i.offset)
	if !ok {
		return 0
	}
	epoch := i.epoch.enter()
	defer i.epoch.exit(epoch)
	if i.closed.Load() {
		return 0
	}
	directory := i.directory.Load()
	var estimate uint64
	first := directory.firstOverlapping(lo)
	for index := first; index < len(directory.entries); index++ {
		entry := &directory.entries[index]
		if entry.startMarker >= hi {
			break
		}
		estimate = indexutil.SaturatingAdd(estimate, estimateBlock(entry.block.view.Load(), lo, hi))
	}
	return estimate
}

func estimateBlock(view *TimestampBlockView, lo, hi uint64) uint64 {
	active := view.Active
	for {
		version := active.stableVersion()
		baseStart, baseEnd := view.Base.rangeIndexes(lo, hi)
		estimate := uint64(baseEnd - baseStart)
		if view.Frozen != nil {
			estimate = indexutil.SaturatingAdd(estimate, estimateDelta(view.Frozen, lo, hi))
		}
		estimate = indexutil.SaturatingAdd(estimate, estimateDelta(active, lo, hi))
		if active.version.Load() == version {
			return estimate
		}
	}
}

func estimateDelta(delta *TimestampDelta, lo, hi uint64) uint64 {
	var estimate uint64
	count := int(delta.addCount.Load())
	for index := 0; index < count; index++ {
		marker := uint64(delta.AddMarkers[index])
		if marker >= lo && marker < hi {
			estimate++
		}
	}
	return estimate
}

func searchBlock(
	dst []ImageID,
	view *TimestampBlockView,
	lo, hi uint64,
	exactStart, exactEnd uint64,
	lookup ExactTimestampFunc,
) []ImageID {
	active := view.Active
	for {
		version := active.stableVersion()
		startLength := len(dst)
		dst = scanBlock(dst, view, lo, hi, exactStart, exactEnd, lookup)
		if active.version.Load() == version {
			return dst
		}
		dst = dst[:startLength]
	}
}

func scanBlock(
	dst []ImageID,
	view *TimestampBlockView,
	lo, hi uint64,
	exactStart, exactEnd uint64,
	lookup ExactTimestampFunc,
) []ImageID {
	baseStart, baseEnd := view.Base.rangeIndexes(lo, hi)
	for index := baseStart; index < baseEnd; index++ {
		id := view.Base.ImageIDs[index]
		if (view.Frozen != nil && view.Frozen.hasState(id)) || view.Active.hasState(id) {
			continue
		}
		dst = appendIfExact(dst, id, exactStart, exactEnd, lookup)
	}

	if view.Frozen != nil {
		count := int(view.Frozen.addCount.Load())
		for index := 0; index < count; index++ {
			marker := uint64(view.Frozen.AddMarkers[index])
			if marker < lo || marker >= hi {
				continue
			}
			id := view.Frozen.AddImageIDs[index]
			if !view.Frozen.latestAdd(id, uint32(index)) || view.Active.hasState(id) {
				continue
			}
			dst = appendIfExact(dst, id, exactStart, exactEnd, lookup)
		}
	}

	count := int(view.Active.addCount.Load())
	for index := 0; index < count; index++ {
		marker := uint64(view.Active.AddMarkers[index])
		if marker < lo || marker >= hi {
			continue
		}
		id := view.Active.AddImageIDs[index]
		if !view.Active.latestAdd(id, uint32(index)) {
			continue
		}
		dst = appendIfExact(dst, id, exactStart, exactEnd, lookup)
	}
	return dst
}

func appendIfExact(
	dst []ImageID,
	id ImageID,
	start, end uint64,
	lookup ExactTimestampFunc,
) []ImageID {
	if lookup == nil {
		return append(dst, id)
	}
	timestamp, ok := lookup(id)
	if ok && timestamp >= start && timestamp < end {
		return append(dst, id)
	}
	return dst
}
