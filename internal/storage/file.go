package storage

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func WriteFull(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

// WriteFileAtomic writes and syncs a temporary file, renames it, then syncs
// the containing directory. temporaryName and finalName are base names.
func WriteFileAtomic(dir, temporaryName, finalName string, data []byte, permissions fs.FileMode) error {
	return WriteFileAtomicFunc(dir, temporaryName, finalName, permissions, func(writer io.Writer) error {
		return WriteFull(writer, data)
	})
}

// WriteFileAtomicFunc is WriteFileAtomic for callers that stream their
// content, such as generation files written plane by plane. The write
// callback receives the temporary file before it is synced and renamed.
func WriteFileAtomicFunc(dir, temporaryName, finalName string, permissions fs.FileMode, write func(io.Writer) error) error {
	temporary := filepath.Join(dir, temporaryName)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions)
	if err != nil {
		return err
	}
	if err = write(file); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, filepath.Join(dir, finalName)); err != nil {
		return err
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory so newly renamed names are durable; callers
// publish a manifest reference only after the names it points at are synced.
func SyncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
}

// DirSize sums the sizes of every file matching one of patterns under
// dir. Patterns may be globs or literal file names; missing files are
// skipped, so retired generations simply stop contributing.
func DirSize(dir string, patterns ...string) uint64 {
	var total uint64
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil {
				total += uint64(info.Size())
			}
		}
	}
	return total
}

func Align(value, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}

func CheckedEnd(offset, count, stride, limit uint64) (uint64, bool) {
	if count != 0 && stride > (^uint64(0)-offset)/count {
		return 0, false
	}
	end := offset + count*stride
	return end, end >= offset && end <= limit
}
