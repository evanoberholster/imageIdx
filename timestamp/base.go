package timestamp

import "sort"

type TimestampBase struct {
	Markers  []Timemarker
	ImageIDs []ImageID
}

func NewTimestampBase(markers []Timemarker, imageIDs []ImageID) (*TimestampBase, error) {
	if len(markers) != len(imageIDs) {
		return nil, ErrInvalidBase
	}
	for i := 1; i < len(markers); i++ {
		if markers[i] < markers[i-1] || (markers[i] == markers[i-1] && imageIDs[i] < imageIDs[i-1]) {
			return nil, ErrInvalidBase
		}
	}
	return &TimestampBase{Markers: markers, ImageIDs: imageIDs}, nil
}

func (b *TimestampBase) Len() int {
	if b == nil {
		return 0
	}
	return len(b.Markers)
}

func (b *TimestampBase) rangeIndexes(lo, hi uint64) (int, int) {
	if b == nil || lo >= hi {
		return 0, 0
	}
	start := sort.Search(len(b.Markers), func(i int) bool {
		return uint64(b.Markers[i]) >= lo
	})
	end := sort.Search(len(b.Markers), func(i int) bool {
		return uint64(b.Markers[i]) >= hi
	})
	return start, end
}
