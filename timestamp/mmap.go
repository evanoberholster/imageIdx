package timestamp

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var ErrUnsupportedEndian = errors.New("timestamp: mmap format requires a little-endian host")

func mmapFile(path string) ([]byte, error) {
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

func unmapFile(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return syscall.Munmap(data)
}

func hostIsLittleEndian() bool {
	value := uint16(1)
	return *(*byte)(unsafe.Pointer(&value)) == 1
}

func mappedMarkers(data []byte, offset uint64, count uint32) ([]Timemarker, error) {
	if !hostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	end := offset + uint64(count)*4
	if end < offset || end > uint64(len(data)) {
		return nil, ErrInvalidBase
	}
	if count == 0 {
		return nil, nil
	}
	result := unsafe.Slice((*Timemarker)(unsafe.Pointer(&data[offset])), int(count))
	runtime.KeepAlive(data)
	return result, nil
}

func mappedImageIDs(data []byte, offset uint64, count uint32) ([]ImageID, error) {
	if !hostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	end := offset + uint64(count)*8
	if end < offset || end > uint64(len(data)) {
		return nil, ErrInvalidBase
	}
	if count == 0 {
		return nil, nil
	}
	result := unsafe.Slice((*ImageID)(unsafe.Pointer(&data[offset])), int(count))
	runtime.KeepAlive(data)
	return result, nil
}
