package persist

import (
	"errors"
	"unsafe"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// MapFile mmaps path read-only. Empty mappings are rejected with emptyErr;
// pass nil to preserve pass-through behavior for formats that validate size
// themselves.
func MapFile(path string, emptyErr error) ([]byte, error) {
	data, err := indexutil.MmapFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 && emptyErr != nil {
		return nil, emptyErr
	}
	return data, nil
}

// Unmap releases a mapping acquired by MapFile.
func Unmap(data []byte) error {
	return indexutil.UnmapFile(data)
}

// View maps count fixed-width values at offset, translating failures to the
// caller's corruption errors so indexutil sentinels never leak across
// package boundaries.
func View[T indexutil.FixedWidth](data []byte, offset, count uint64, corruptErr, endianErr error) ([]T, error) {
	result, err := indexutil.MappedSlice[T](data, offset, count)
	if errors.Is(err, indexutil.ErrUnsupportedEndian) {
		return nil, endianErr
	}
	if err != nil {
		return nil, corruptErr
	}
	return result, nil
}

// ViewFull maps the whole span, rejecting trailing partial words that a
// truncated count would silently ignore.
func ViewFull[T indexutil.FixedWidth](data []byte, corruptErr, endianErr error) ([]T, error) {
	var zero T
	width := uint64(unsafe.Sizeof(zero))
	if uint64(len(data))%width != 0 {
		return nil, corruptErr
	}
	return View[T](data, 0, uint64(len(data))/width, corruptErr, endianErr)
}
