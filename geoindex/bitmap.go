package geoindex

import (
	"sort"

	"github.com/evanoberholster/imageIdx/bitmapidx"
)

type GeoBitmapMeta struct {
	Key         S2Key
	Cardinality uint64
	Bitmap      bitmapidx.BitmapRef
}

type GeoBitmapRef struct {
	keys    []S2Key
	bitmaps []bitmapidx.BitmapRef
	backing []byte
}

func NewGeoBitmapRef(entries []GeoBitmapMeta) (GeoBitmapRef, error) {
	copyEntries := append([]GeoBitmapMeta(nil), entries...)
	sort.Slice(copyEntries, func(i, j int) bool { return copyEntries[i].Key < copyEntries[j].Key })
	result := GeoBitmapRef{keys: make([]S2Key, len(copyEntries)), bitmaps: make([]bitmapidx.BitmapRef, len(copyEntries))}
	for index, entry := range copyEntries {
		if index != 0 && entry.Key == copyEntries[index-1].Key || entry.Bitmap.Cardinality() < GeoSparseSlots+1 ||
			entry.Cardinality != 0 && entry.Cardinality != entry.Bitmap.Cardinality() {
			return GeoBitmapRef{}, ErrCorruptStorage
		}
		if _, err := CellIDFromS2Key(entry.Key); err != nil {
			return GeoBitmapRef{}, err
		}
		result.keys[index] = entry.Key
		result.bitmaps[index] = entry.Bitmap
	}
	return result, nil
}

func (b GeoBitmapRef) CellCount() int { return len(b.keys) }
func (b GeoBitmapRef) Keys() []S2Key  { return append([]S2Key(nil), b.keys...) }

func (b GeoBitmapRef) Lookup(key S2Key) (bitmapidx.BitmapRef, bool) {
	index := lowerBound(b.keys, key)
	if index == len(b.keys) || b.keys[index] != key {
		return bitmapidx.BitmapRef{}, false
	}
	return b.bitmaps[index], true
}

func (b GeoBitmapRef) SearchRanges(dst []ImageID, ranges []S2KeyRange) []ImageID {
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

func buildBitmap(cells map[S2Key][]uint64) GeoBitmapRef {
	keys := make([]S2Key, 0, len(cells))
	for key, ids := range cells {
		if len(ids) > GeoSparseSlots {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	bitmaps := make([]bitmapidx.BitmapRef, len(keys))
	for index, key := range keys {
		bitmaps[index] = bitmapidx.NewBitmapRef(cells[key])
	}
	return GeoBitmapRef{keys: keys, bitmaps: bitmaps}
}
