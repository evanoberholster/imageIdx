package geoindex

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var ErrUnsupportedEndian = errors.New("geoindex: mmap format requires a little-endian host")

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
		return nil, ErrCorruptStorage
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

func mappedS2Keys(data []byte) ([]S2Key, error) {
	if !hostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	if len(data)%8 != 0 {
		return nil, ErrCorruptStorage
	}
	if len(data) == 0 {
		return nil, nil
	}
	result := unsafe.Slice((*S2Key)(unsafe.Pointer(&data[0])), len(data)/8)
	runtime.KeepAlive(data)
	return result, nil
}

func mappedUint64(data []byte) ([]uint64, error) {
	if !hostIsLittleEndian() {
		return nil, ErrUnsupportedEndian
	}
	if len(data)%8 != 0 {
		return nil, ErrCorruptStorage
	}
	if len(data) == 0 {
		return nil, nil
	}
	result := unsafe.Slice((*uint64)(unsafe.Pointer(&data[0])), len(data)/8)
	runtime.KeepAlive(data)
	return result, nil
}
