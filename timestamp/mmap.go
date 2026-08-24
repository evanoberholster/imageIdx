package timestamp

import (
	"errors"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

var ErrUnsupportedEndian = errors.New("timestamp: mmap format requires a little-endian host")

func mmapFile(path string) ([]byte, error) {
	return indexutil.MmapFile(path)
}

func unmapFile(data []byte) error {
	return indexutil.UnmapFile(data)
}

func mappedMarkers(data []byte, offset uint64, count uint32) ([]timemarker, error) {
	result, err := indexutil.MappedSlice[timemarker](data, offset, uint64(count))
	return mapMappedError(result, err)
}

func mappedImageIDs(data []byte, offset uint64, count uint32) ([]ImageID, error) {
	result, err := indexutil.MappedSlice[ImageID](data, offset, uint64(count))
	return mapMappedError(result, err)
}

func mapMappedError[T indexutil.FixedWidth](result []T, err error) ([]T, error) {
	if errors.Is(err, indexutil.ErrUnsupportedEndian) {
		return nil, ErrUnsupportedEndian
	}
	if err != nil {
		return nil, ErrInvalidBase
	}
	return result, nil
}
