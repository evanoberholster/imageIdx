package geoindex

import (
	"errors"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

var ErrUnsupportedEndian = errors.New("geoindex: mmap format requires a little-endian host")

func mmapFile(path string) ([]byte, error) {
	data, err := indexutil.MmapFile(path)
	if err == nil && len(data) == 0 {
		return nil, ErrCorruptStorage
	}
	return data, err
}

func unmapFile(data []byte) error {
	return indexutil.UnmapFile(data)
}

func mappedS2Keys(data []byte) ([]S2Key, error) {
	if !indexutil.HostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	if len(data)%8 != 0 {
		return nil, ErrCorruptStorage
	}
	result, err := indexutil.MappedSlice[S2Key](data, 0, uint64(len(data)/8))
	return mapMappedError(result, err)
}

func mappedUint64(data []byte) ([]uint64, error) {
	if !indexutil.HostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	if len(data)%8 != 0 {
		return nil, ErrCorruptStorage
	}
	result, err := indexutil.MappedSlice[uint64](data, 0, uint64(len(data)/8))
	return mapMappedError(result, err)
}

func mapMappedError[T indexutil.FixedWidth](result []T, err error) ([]T, error) {
	if errors.Is(err, indexutil.ErrUnsupportedEndian) {
		return nil, ErrUnsupportedEndian
	}
	if err != nil {
		return nil, ErrCorruptStorage
	}
	return result, nil
}
