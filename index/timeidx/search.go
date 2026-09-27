package timeidx

import "github.com/evanoberholster/imageIdx/internal/storage"

type ExactTimestampFunc func(ImageID) (uint64, bool)

// searchScope resolves the marker range and pins the directory for
// lock-free readers, returning the release token alongside. The caller
// must pass the token to Release exactly once; results accumulate into
// the caller's dst, which it sizes. Empty ranges and closed indexes
// report ok=false with no reader registered and no allocation.
func (i *Index) searchScope(start, end uint64) (directory *blockDirectory, lo, hi uint64, token uint32, ok bool) {
	lo, hi, ok = markerRange(start, end, i.offset)
	if !ok {
		return nil, 0, 0, 0, false
	}
	token = i.readers.Acquire()
	if i.closed.Load() {
		i.readers.Release(token)
		return nil, 0, 0, 0, false
	}
	return i.directory.Load(), lo, hi, token, true
}

// Search appends compressed-index candidates for [start, end) to dst. The
// caller controls allocation by supplying enough spare capacity in dst.
func (i *Index) Search(dst []ImageID, start, end uint64) []ImageID {
	directory, lo, hi, token, ok := i.searchScope(start, end)
	if !ok {
		return dst
	}
	for index := directory.firstOverlapping(lo); index < len(directory.entries); index++ {
		entry := &directory.entries[index]
		if entry.startMarker >= hi {
			break
		}
		view := entry.block.view.Load()
		dst = searchBlock(dst, view, lo, hi, 0, 0, nil)
	}
	i.readers.Release(token)
	return dst
}

// SearchExact filters lossy compressed candidates through the canonical
// timestamp lookup. The lookup must be safe for concurrent reads.
func (i *Index) SearchExact(dst []ImageID, start, end uint64, lookup ExactTimestampFunc) []ImageID {
	if lookup == nil {
		return i.Search(dst, start, end)
	}
	directory, lo, hi, token, ok := i.searchScope(start, end)
	if !ok {
		return dst
	}
	for index := directory.firstOverlapping(lo); index < len(directory.entries); index++ {
		entry := &directory.entries[index]
		if entry.startMarker >= hi {
			break
		}
		view := entry.block.view.Load()
		dst = searchBlock(dst, view, lo, hi, start, end, lookup)
	}
	i.readers.Release(token)
	return dst
}

// EstimateRange returns an upper bound for compressed-index candidates in
// [start, end). It inspects marker counts but does not materialize ImageIDs,
// making it suitable for cross-index query planning. Delta deletes and repeated
// updates may make the estimate larger than the eventual result.
func (i *Index) EstimateRange(start, end uint64) uint64 {
	directory, lo, hi, token, ok := i.searchScope(start, end)
	if !ok {
		return 0
	}
	var estimate uint64
	for index := directory.firstOverlapping(lo); index < len(directory.entries); index++ {
		entry := &directory.entries[index]
		if entry.startMarker >= hi {
			break
		}
		estimate = storage.SaturatingAdd(estimate, estimateBlock(entry.block.view.Load(), lo, hi))
	}
	i.readers.Release(token)
	return estimate
}

func estimateBlock(view *blockView, lo, hi uint64) uint64 {
	active := view.active
	for {
		version := active.stableVersion()
		baseStart, baseEnd := view.base.rangeIndexes(lo, hi)
		estimate := uint64(baseEnd - baseStart)
		if view.frozen != nil {
			estimate = storage.SaturatingAdd(estimate, estimateDelta(view.frozen, lo, hi))
		}
		estimate = storage.SaturatingAdd(estimate, estimateDelta(active, lo, hi))
		if active.version.Load() == version {
			return estimate
		}
	}
}

func estimateDelta(delta *delta, lo, hi uint64) uint64 {
	var estimate uint64
	count := int(delta.addCount.Load())
	for index := 0; index < count; index++ {
		marker := uint64(delta.addMarkers[index])
		if marker >= lo && marker < hi {
			estimate++
		}
	}
	return estimate
}

func searchBlock(
	dst []ImageID,
	view *blockView,
	lo, hi uint64,
	exactStart, exactEnd uint64,
	lookup ExactTimestampFunc,
) []ImageID {
	active := view.active
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
	view *blockView,
	lo, hi uint64,
	exactStart, exactEnd uint64,
	lookup ExactTimestampFunc,
) []ImageID {
	baseStart, baseEnd := view.base.rangeIndexes(lo, hi)
	for index := baseStart; index < baseEnd; index++ {
		id := view.base.imageIDs[index]
		if (view.frozen != nil && view.frozen.hasState(id)) || view.active.hasState(id) {
			continue
		}
		dst = appendIfExact(dst, id, exactStart, exactEnd, lookup)
	}

	if view.frozen != nil {
		count := int(view.frozen.addCount.Load())
		for index := 0; index < count; index++ {
			marker := uint64(view.frozen.addMarkers[index])
			if marker < lo || marker >= hi {
				continue
			}
			id := view.frozen.addImageIDs[index]
			if !view.frozen.latestAdd(id, uint32(index)) || view.active.hasState(id) {
				continue
			}
			dst = appendIfExact(dst, id, exactStart, exactEnd, lookup)
		}
	}

	count := int(view.active.addCount.Load())
	for index := 0; index < count; index++ {
		marker := uint64(view.active.addMarkers[index])
		if marker < lo || marker >= hi {
			continue
		}
		id := view.active.addImageIDs[index]
		if !view.active.latestAdd(id, uint32(index)) {
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
