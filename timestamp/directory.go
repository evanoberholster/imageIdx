package timestamp

import "sort"

const markerSpaceEnd uint64 = 1 << 32

type blockEntry struct {
	startMarker uint64
	endMarker   uint64
	meta        blockMetadata
	block       *block
}

type blockDirectory struct {
	entries []blockEntry
}

func (d *blockDirectory) firstOverlapping(marker uint64) int {
	return sort.Search(len(d.entries), func(i int) bool {
		return d.entries[i].endMarker > marker
	})
}

func (d *blockDirectory) blockFor(marker uint64) int {
	if len(d.entries) == 1 {
		return 0
	}
	index := sort.Search(len(d.entries), func(i int) bool {
		return d.entries[i].endMarker > marker
	})
	if index == len(d.entries) {
		return len(d.entries) - 1
	}
	return index
}

func timestampForBoundary(marker, offset uint64) uint64 {
	if marker == 0 {
		return offset
	}
	delta := marker << TimestampShift
	if delta > ^uint64(0)-offset {
		return ^uint64(0)
	}
	return offset + delta
}
