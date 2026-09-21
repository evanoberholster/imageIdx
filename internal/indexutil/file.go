package indexutil

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
	return writeFileAtomic(dir, temporaryName, finalName, data, permissions, WriteFull)
}

// WriteFileAtomicOnce preserves callers whose on-disk publication path uses a
// single os.File.Write operation. Prefer WriteFileAtomic for new formats.
func WriteFileAtomicOnce(dir, temporaryName, finalName string, data []byte, permissions fs.FileMode) error {
	return writeFileAtomic(dir, temporaryName, finalName, data, permissions, func(writer io.Writer, data []byte) error {
		_, err := writer.Write(data)
		return err
	})
}

func writeFileAtomic(
	dir, temporaryName, finalName string,
	data []byte,
	permissions fs.FileMode,
	write func(io.Writer, []byte) error,
) error {
	temporary := filepath.Join(dir, temporaryName)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permissions)
	if err != nil {
		return err
	}
	if err = write(file, data); err == nil {
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
