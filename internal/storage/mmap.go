package storage

import (
	"os"
	"runtime"
	"unsafe"
)

// FixedWidth constrains the word types index formats may map directly.
type FixedWidth interface {
	~uint32 | ~uint64
}

// MapFile mmaps path read-only. A zero-length file yields nil data unless
// emptyErr is non-nil, in which case emptyErr is returned; pass nil for
// formats that validate size themselves. Platform backends live in
// mmap_unix.go, mmap_windows.go, and mmap_other.go.
func MapFile(path string, emptyErr error) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		if emptyErr != nil {
			return nil, emptyErr
		}
		return nil, nil
	}
	if info.Size() < 0 || uint64(info.Size()) > uint64(^uint(0)>>1) {
		return nil, ErrMappedSize
	}
	return mmapFile(file, info.Size())
}

// Unmap releases a mapping acquired by MapFile. A nil mapping is a no-op.
func Unmap(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return unmapFile(data)
}

// HostIsLittleEndian reports whether the host byte order matches every
// persisted index format.
func HostIsLittleEndian() bool {
	value := uint16(1)
	return *(*byte)(unsafe.Pointer(&value)) == 1
}

// View maps count fixed-width values at offset, translating failures to the
// caller's corruption errors so storage sentinels never leak across package
// boundaries. count zero returns a nil view.
func View[T FixedWidth](data []byte, offset, count uint64, corruptErr, endianErr error) ([]T, error) {
	if !HostIsLittleEndian() {
		return nil, endianErr
	}
	var zero T
	width := uint64(unsafe.Sizeof(zero))
	if _, ok := CheckedEnd(offset, count, width, uint64(len(data))); !ok {
		return nil, corruptErr
	}
	if count == 0 {
		return nil, nil
	}
	result := unsafe.Slice((*T)(unsafe.Pointer(&data[offset])), int(count))
	runtime.KeepAlive(data)
	return result, nil
}
