//go:build unix

package storage

import (
	"os"
	"syscall"
)

func mmapFile(file *os.File, size int64) ([]byte, error) {
	return syscall.Mmap(int(file.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
}

func unmapFile(data []byte) error {
	return syscall.Munmap(data)
}
