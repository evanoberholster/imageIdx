package geoidx

import (
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/roar"
)

type bitmapRef struct {
	keys    []S2Key
	bitmaps []roar.Bitmap
	backing []byte
}

func (b bitmapRef) cellCount() int { return len(b.keys) }

func (b bitmapRef) lookup(key S2Key) (roar.Bitmap, bool) {
	index := lowerBound(b.keys, key)
	if index == len(b.keys) || b.keys[index] != key {
		return roar.Bitmap{}, false
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

func (b bitmapRef) filterRanges(dst, candidates []ImageID, ranges []Range) []ImageID {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(b.keys) && b.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(b.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(b.keys) && b.keys[keyIndex] <= keyRange.End {
			dst = b.bitmaps[keyIndex].FilterSorted(dst, candidates)
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

// collectRanges folds covered dense cells into the transient MutableBitmap
// m without decoding them to IDs. The caller resets m first and materializes
// once with AppendTo.
func (b bitmapRef) collectRanges(m *roar.MutableBitmap, ranges []Range) {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(b.keys) && b.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(b.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(b.keys) && b.keys[keyIndex] <= keyRange.End {
			m.Or(b.bitmaps[keyIndex])
			keyIndex++
		}
	}
}

func buildBitmap(cells map[S2Key][]ImageID) bitmapRef {
	keys := make([]S2Key, 0, len(cells))
	for key, ids := range cells {
		if len(ids) > geoSparseSlots {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	bitmaps := make([]roar.Bitmap, len(keys))
	for index, key := range keys {
		bitmaps[index] = roar.FromUnsorted(cells[key])
	}
	return bitmapRef{keys: keys, bitmaps: bitmaps}
}
