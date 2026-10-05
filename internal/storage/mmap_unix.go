//go:build unix

package storage

import (
	"os"

	"golang.org/x/sys/unix"
)

func mmapFile(file *os.File, size int64) ([]byte, error) {
	return unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
}

func unmapFile(data []byte) error {
	return unix.Munmap(data)
}

// mmapFileRW maps a file read-write and shared so writes reach the file.
func mmapFileRW(file *os.File, size int64) ([]byte, error) {
	return unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
}

func msyncMapped(data []byte) error { return unix.Msync(data, unix.MS_SYNC) }

func munmapMapped(data []byte) error { return unix.Munmap(data) }
