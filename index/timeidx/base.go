package timeidx

import (
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
)

type base struct {
	markers  []timemarker
	imageIDs []common.ValueID
}

func newBase(markers []timemarker, imageIDs []common.ValueID) (*base, error) {
	if len(markers) != len(imageIDs) {
		return nil, ErrInvalidBase
	}
	for i := 1; i < len(markers); i++ {
		if markers[i] < markers[i-1] || (markers[i] == markers[i-1] && imageIDs[i] < imageIDs[i-1]) {
			return nil, ErrInvalidBase
		}
	}
	return &base{markers: markers, imageIDs: imageIDs}, nil
}

func (b *base) len() int {
	if b == nil {
		return 0
	}
	return len(b.markers)
}

func (b *base) rangeIndexes(lo, hi uint64) (int, int) {
	if b == nil || lo >= hi {
		return 0, 0
	}
	start, _ := slices.BinarySearchFunc(b.markers, lo, func(marker timemarker, bound uint64) int {
		if uint64(marker) < bound {
			return -1
		}
		return 1
	})
	end, _ := slices.BinarySearchFunc(b.markers, hi, func(marker timemarker, bound uint64) int {
		if uint64(marker) < bound {
			return -1
		}
		return 1
	})
	return start, end
}
