//go:build !unix && !windows

package storage

import (
	"os"
)

// mmapFile fails fast where neither mmap nor MapViewOfFile exists. Proceeding
// without memory mapping would silently change the read representation.
func mmapFile(_ *os.File, _ int64) ([]byte, error) {
	return nil, errMmapUnsupported
}

// unmapFile has no mapping to release on unsupported platforms.
func unmapFile(_ []byte) error {
	return nil
}

// mmapFileRW fails fast where no read-write mapping exists.
func mmapFileRW(_ *os.File, _ int64) ([]byte, error) { return nil, errMmapUnsupported }

func msyncMapped(_ []byte) error { return errMmapUnsupported }

func munmapMapped(_ []byte) error { return nil }
