package geoindex

import (
	"sort"

	"github.com/evanoberholster/imageIdx/idset"
	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

type bitmapRef struct {
	keys    []S2Key
	bitmaps []idset.Ref
	backing []byte
}

func (b bitmapRef) cellCount() int { return len(b.keys) }

func (b bitmapRef) lookup(key S2Key) (idset.Ref, bool) {
	index := lowerBound(b.keys, key)
	if index == len(b.keys) || b.keys[index] != key {
		return idset.Ref{}, false
	}
	return b.bitmaps[index], true
}

func (b bitmapRef) searchRanges(dst []ImageID, ranges []Range) []ImageID {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(b.keys) && b.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(b.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(b.keys) && b.keys[keyIndex] <= keyRange.End {
			dst = b.bitmaps[keyIndex].AppendTo(dst)
			keyIndex++
		}
	}
	return dst
}

func (b bitmapRef) estimateRanges(ranges []Range) uint64 {
	var count uint64
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(b.keys) && b.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(b.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(b.keys) && b.keys[keyIndex] <= keyRange.End {
			count = indexutil.SaturatingAdd(count, b.bitmaps[keyIndex].Cardinality())
			keyIndex++
		}
	}
	return count
}

func buildBitmap(cells map[S2Key][]ImageID) bitmapRef {
	keys := make([]S2Key, 0, len(cells))
	for key, ids := range cells {
		if len(ids) > geoSparseSlots {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	bitmaps := make([]idset.Ref, len(keys))
	for index, key := range keys {
		bitmaps[index] = idset.New(cells[key])
	}
	return bitmapRef{keys: keys, bitmaps: bitmaps}
}
