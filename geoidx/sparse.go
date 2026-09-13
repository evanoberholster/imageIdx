package geoidx

import (
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/roar"
)

type sparseRef struct {
	keys     []S2Key
	imageIDs []uint64
	backing  []byte
}

func (s sparseRef) cellCount() int { return len(s.keys) }

func (s sparseRef) contains(key S2Key, id ImageID) bool {
	index := lowerBound(s.keys, key)
	if index == len(s.keys) || s.keys[index] != key {
		return false
	}
	for _, value := range s.imageIDs[index*geoSparseSlots : (index+1)*geoSparseSlots] {
		if value == 0 {
			break
		}
		if ImageID(value) == id {
			return true
		}
	}
	return false
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

func (s sparseRef) filterRanges(dst, candidates []ImageID, ranges []Range) []ImageID {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(s.keys) && s.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(s.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(s.keys) && s.keys[keyIndex] <= keyRange.End {
			dst = filterSparseIDs(dst, candidates, s.imageIDs[keyIndex*geoSparseSlots:(keyIndex+1)*geoSparseSlots])
			keyIndex++
		}
	}
	return dst
}

// filterSparseIDs appends the candidates present in one sparse slot. Both
// inputs arrive ascending, so a two-pointer walk filters without allocating.
func filterSparseIDs(dst, candidates []ImageID, values []uint64) []ImageID {
	slot, candidate := 0, 0
	for slot < len(values) && values[slot] != 0 && candidate < len(candidates) {
		id := ImageID(values[slot])
		switch {
		case candidates[candidate] < id:
			candidate++
		case candidates[candidate] > id:
			slot++
		default:
			dst = append(dst, id)
			slot++
			candidate++
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

// collectRanges stages covered sparse slots, stopping at zero padding, into
// the transient MutableBitmap m. Duplicates canonicalize away at
// materialization.
func (s sparseRef) collectRanges(m *roar.MutableBitmap, ranges []Range) {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(s.keys) && s.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(s.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(s.keys) && s.keys[keyIndex] <= keyRange.End {
			offset := keyIndex * geoSparseSlots
			end := offset
			for end < offset+geoSparseSlots && s.imageIDs[end] != 0 {
				end++
			}
			m.AddMany(s.imageIDs[offset:end])
			keyIndex++
		}
	}
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
