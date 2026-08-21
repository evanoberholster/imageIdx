package bitmapidx

import (
	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

func mmapFile(path string) ([]byte, error) {
	data, err := indexutil.MmapFile(path)
	if err == nil && len(data) == 0 {
		return nil, ErrCorruptSegment
	}
	return data, err
}

func unmapFile(data []byte) error {
	return indexutil.UnmapFile(data)
}
