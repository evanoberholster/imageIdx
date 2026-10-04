package storage

import (
	"errors"
	"os"
)

// ErrMappedSize reports a non-positive mapping size.
var ErrMappedSize = errors.New("storage: mapped file size must be positive")

// errMmapUnsupported reports a platform without the requested mapping mode.
var errMmapUnsupported = errors.New("storage: memory mapping unsupported on this platform")

// MappedFile is a read-write memory mapping of a whole file. Writes reach the
// file through the shared mapping; Sync flushes both. CreateMapped truncates an
// existing file, so a fresh mapping starts zero-filled.
type MappedFile struct {
	file *os.File
	data []byte
}

// CreateMapped creates or truncates path to size bytes and maps it read-write.
// The file is sparse until written, so a large logical arena does not consume
// disk or resident memory up front. Platform backends that lack a read-write
// mapping fail with errMmapUnsupported.
func CreateMapped(path string, size int64) (*MappedFile, error) {
	if size <= 0 {
		return nil, ErrMappedSize
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return nil, err
	}
	data, err := mmapFileRW(file, size)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &MappedFile{file: file, data: data}, nil
}

// Bytes returns the mapped region. It is nil after Close.
func (m *MappedFile) Bytes() []byte {
	if m == nil {
		return nil
	}
	return m.data
}

// Sync flushes the mapping and the underlying file to stable storage.
func (m *MappedFile) Sync() error {
	if m == nil || m.data == nil || m.file == nil {
		return nil
	}
	if err := msyncMapped(m.data); err != nil {
		return err
	}
	return m.file.Sync()
}

// Close unmaps the file and closes it. It is safe to call more than once.
func (m *MappedFile) Close() error {
	if m == nil {
		return nil
	}
	var err error
	if m.data != nil {
		err = munmapMapped(m.data)
		m.data = nil
	}
	if m.file != nil {
		if closeErr := m.file.Close(); err == nil {
			err = closeErr
		}
		m.file = nil
	}
	return err
}
