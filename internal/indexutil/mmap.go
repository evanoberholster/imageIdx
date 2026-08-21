package indexutil

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	ErrInvalidMapping    = errors.New("indexutil: invalid mapped range")
	ErrUnsupportedEndian = errors.New("indexutil: mmap formats require a little-endian host")
)

type FixedWidth interface {
	~uint32 | ~uint64
}

func MmapFile(path string) ([]byte, error) {
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
		return nil, nil
	}
	return syscall.Mmap(int(file.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
}

func UnmapFile(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return syscall.Munmap(data)
}

func HostIsLittleEndian() bool {
	value := uint16(1)
	return *(*byte)(unsafe.Pointer(&value)) == 1
}

func MappedSlice[T FixedWidth](data []byte, offset, count uint64) ([]T, error) {
	if !HostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	var zero T
	width := uint64(unsafe.Sizeof(zero))
	_, ok := CheckedEnd(offset, count, width, uint64(len(data)))
	if !ok {
		return nil, ErrInvalidMapping
	}
	if count == 0 {
		return nil, nil
	}
	result := unsafe.Slice((*T)(unsafe.Pointer(&data[offset])), int(count))
	runtime.KeepAlive(data)
	return result, nil
}
