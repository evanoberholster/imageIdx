//go:build windows

package storage

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mmapFile maps the whole file read-only. The mapping handle can be closed
// once the view exists; the view stays valid until Unmap.
func mmapFile(file *os.File, size int64) ([]byte, error) {
	mapping, err := windows.CreateFileMapping(windows.Handle(file.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(mapping)
	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(size)), nil
}

func unmapFile(data []byte) error {
	return windows.UnmapViewOfFile(uintptr(unsafe.Pointer(&data[0])))
}
