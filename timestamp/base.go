package timestamp

import "sort"

type base struct {
	markers  []timemarker
	imageIDs []ImageID
}

func newBase(markers []timemarker, imageIDs []ImageID) (*base, error) {
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
	start := sort.Search(len(b.markers), func(i int) bool {
		return uint64(b.markers[i]) >= lo
	})
	end := sort.Search(len(b.markers), func(i int) bool {
		return uint64(b.markers[i]) >= hi
	})
	return start, end
}
