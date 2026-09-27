//go:build !unix && !windows

package storage

import (
	"errors"
	"os"
)

var errMmapUnsupported = errors.New("storage: memory mapping unsupported on this platform")

// mmapFile fails fast where neither mmap nor MapViewOfFile exists. Proceeding
// without memory mapping would silently change the read representation.
func mmapFile(_ *os.File, _ int64) ([]byte, error) {
	return nil, errMmapUnsupported
}

// unmapFile has no mapping to release on unsupported platforms.
func unmapFile(_ []byte) error {
	return nil
}
