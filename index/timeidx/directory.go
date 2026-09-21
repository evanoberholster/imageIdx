package timeidx

import "slices"

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

// firstOverlapping returns the first block whose range extends past
// marker. Blocks tile [0, markerSpaceEnd) contiguously, so every marker
// below the space end overlaps exactly one block.
func (d *blockDirectory) firstOverlapping(marker uint64) int {
	index, _ := slices.BinarySearchFunc(d.entries, marker, func(entry blockEntry, bound uint64) int {
		if entry.endMarker <= bound {
			return -1
		}
		return 1
	})
	return index
}

func (d *blockDirectory) blockFor(marker uint64) int {
	if len(d.entries) == 1 {
		return 0
	}
	if index := d.firstOverlapping(marker); index != len(d.entries) {
		return index
	}
	return len(d.entries) - 1
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
