package geoindex

import (
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

type sparseRef struct {
	keys     []S2Key
	imageIDs []uint64
	backing  []byte
}

func (s sparseRef) cellCount() int { return len(s.keys) }

func (s sparseRef) lookup(key S2Key, dst []ImageID) []ImageID {
	index := lowerBound(s.keys, key)
	if index == len(s.keys) || s.keys[index] != key {
		return dst
	}
	return appendSparseIDs(dst, s.imageIDs[index*geoSparseSlots:(index+1)*geoSparseSlots])
}

func (s sparseRef) searchRanges(dst []ImageID, ranges []Range) []ImageID {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(s.keys) && s.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(s.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(s.keys) && s.keys[keyIndex] <= keyRange.End {
			offset := keyIndex * geoSparseSlots
			dst = appendSparseIDs(dst, s.imageIDs[offset:offset+geoSparseSlots])
			keyIndex++
		}
	}
	return dst
}

func (s sparseRef) estimateRanges(ranges []Range) uint64 {
	var count uint64
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(s.keys) && s.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(s.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(s.keys) && s.keys[keyIndex] <= keyRange.End {
			offset := keyIndex * geoSparseSlots
			for _, id := range s.imageIDs[offset : offset+geoSparseSlots] {
				if id == 0 {
					break
				}
				count = indexutil.SaturatingAdd(count, 1)
			}
			keyIndex++
		}
	}
	return count
}

func appendSparseIDs(dst []ImageID, values []uint64) []ImageID {
	for _, id := range values {
		if id == 0 {
			break
		}
		dst = append(dst, ImageID(id))
	}
	return dst
}

func validateSparse(keys []S2Key, ids []uint64) error {
	for index, key := range keys {
		if index != 0 && key <= keys[index-1] {
			return ErrCorruptStorage
		}
		if _, err := CellIDFromS2Key(key); err != nil {
			return err
		}
		values := ids[index*geoSparseSlots : (index+1)*geoSparseSlots]
		seenZero := false
		previous := uint64(0)
		for _, id := range values {
			if id == 0 {
				seenZero = true
				continue
			}
			if seenZero || id <= previous {
				return ErrCorruptStorage
			}
			previous = id
		}
	}
	return nil
}

func buildSparse(cells map[S2Key][]ImageID) sparseRef {
	keys := make([]S2Key, 0, len(cells))
	for key, ids := range cells {
		if len(ids) > 0 && len(ids) <= geoSparseSlots {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	values := make([]uint64, len(keys)*geoSparseSlots)
	for index, key := range keys {
		ids := cells[key]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		copy(values[index*geoSparseSlots:], ids)
	}
	return sparseRef{keys: keys, imageIDs: values}
}
