package geoindex

import (
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

type GeoSparseRef struct {
	keys     []S2Key
	imageIDs []uint64
	backing  []byte
}

func NewGeoSparseRef(keys []S2Key, imageIDs []uint64) (GeoSparseRef, error) {
	if len(imageIDs) != len(keys)*GeoSparseSlots {
		return GeoSparseRef{}, ErrCorruptStorage
	}
	keyCopy := append([]S2Key(nil), keys...)
	idCopy := append([]uint64(nil), imageIDs...)
	if err := validateSparse(keyCopy, idCopy); err != nil {
		return GeoSparseRef{}, err
	}
	return GeoSparseRef{keys: keyCopy, imageIDs: idCopy}, nil
}

func (s GeoSparseRef) CellCount() int { return len(s.keys) }
func (s GeoSparseRef) Keys() []S2Key  { return append([]S2Key(nil), s.keys...) }

func (s GeoSparseRef) Lookup(key S2Key, dst []ImageID) []ImageID {
	index := lowerBound(s.keys, key)
	if index == len(s.keys) || s.keys[index] != key {
		return dst
	}
	return appendSparseIDs(dst, s.imageIDs[index*GeoSparseSlots:(index+1)*GeoSparseSlots])
}

func (s GeoSparseRef) SearchRanges(dst []ImageID, ranges []S2KeyRange) []ImageID {
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(s.keys) && s.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(s.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(s.keys) && s.keys[keyIndex] <= keyRange.End {
			offset := keyIndex * GeoSparseSlots
			dst = appendSparseIDs(dst, s.imageIDs[offset:offset+GeoSparseSlots])
			keyIndex++
		}
	}
	return dst
}

func (s GeoSparseRef) estimateRanges(ranges []S2KeyRange) uint64 {
	var count uint64
	keyIndex := 0
	for _, keyRange := range ranges {
		if keyIndex < len(s.keys) && s.keys[keyIndex] < keyRange.Start {
			keyIndex += lowerBound(s.keys[keyIndex:], keyRange.Start)
		}
		for keyIndex < len(s.keys) && s.keys[keyIndex] <= keyRange.End {
			offset := keyIndex * GeoSparseSlots
			for _, id := range s.imageIDs[offset : offset+GeoSparseSlots] {
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
		values := ids[index*GeoSparseSlots : (index+1)*GeoSparseSlots]
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

func buildSparse(cells map[S2Key][]uint64) GeoSparseRef {
	keys := make([]S2Key, 0, len(cells))
	for key, ids := range cells {
		if len(ids) > 0 && len(ids) <= GeoSparseSlots {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	values := make([]uint64, len(keys)*GeoSparseSlots)
	for index, key := range keys {
		ids := cells[key]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		copy(values[index*GeoSparseSlots:], ids)
	}
	return GeoSparseRef{keys: keys, imageIDs: values}
}
