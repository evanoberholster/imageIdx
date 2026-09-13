package persist

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type failingWALFile struct {
	position                       int64
	writes                         int
	writeErr, truncateErr, syncErr error
	short                          bool
}

func (f *failingWALFile) Write(p []byte) (int, error) {
	f.writes++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short {
		return 0, nil
	}
	f.position += int64(len(p))
	return len(p), nil
}
func (f *failingWALFile) Seek(_ int64, _ int) (int64, error) { return f.position, nil }
func (f *failingWALFile) Truncate(n int64) error {
	if f.truncateErr != nil {
		return f.truncateErr
	}
	f.position = n
	return nil
}
func (f *failingWALFile) Sync() error  { return f.syncErr }
func (f *failingWALFile) Close() error { return nil }

func TestWALUncertainFailureIsTerminal(t *testing.T) {
	writeErr := errors.New("write failed")
	truncateErr := errors.New("rollback failed")
	syncErr := errors.New("sync failed")
	for _, test := range []struct {
		name  string
		file  failingWALFile
		cause error
	}{
		{"write", failingWALFile{writeErr: writeErr}, writeErr},
		{"rollback", failingWALFile{writeErr: writeErr, truncateErr: truncateErr}, truncateErr},
		{"sync", failingWALFile{syncErr: syncErr}, syncErr},
		{"short", failingWALFile{short: true}, io.ErrShortWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := &WAL{file: &test.file}
			err := w.AppendFrame([]byte{1}, []byte{2}, true)
			if !errors.Is(err, ErrWALFailed) || !errors.Is(err, test.cause) {
				t.Fatal(err)
			}
			writes := test.file.writes
			test.file.writeErr = nil
			test.file.truncateErr = nil
			test.file.syncErr = nil
			test.file.short = false
			if err := w.AppendFrame([]byte{3}, nil, false); !errors.Is(err, ErrWALFailed) {
				t.Fatal(err)
			}
			if err := w.Sync(); !errors.Is(err, ErrWALFailed) {
				t.Fatal(err)
			}
			if test.file.writes != writes {
				t.Fatal("failed writer accepted later data")
			}
		})
	}
}

func TestReplayEndBoundsBeforeAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal")
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data[:4], 42)
	binary.LittleEndian.PutUint16(data[4:6], 1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := errors.New("corrupt")
	spec := FrameSpec{HeaderSize: 16, Magic: 42, Version: 1, Corrupt: corrupt, MaxPayload: 1024}
	visit := func([]byte, []byte) error { t.Fatal("unexpected visit"); return nil }
	if _, err := ReplayEnd(path, 0, spec, func([]byte) (uint64, error) { return 1 << 40, nil }, visit); !errors.Is(err, corrupt) {
		t.Fatal(err)
	}
	for _, offset := range []int64{-1, 17} {
		if _, err := ReplayEnd(path, offset, spec, func([]byte) (uint64, error) { return 0, nil }, visit); !errors.Is(err, corrupt) {
			t.Fatal(err)
		}
	}
	end, err := ReplayEnd(path, 0, spec, func([]byte) (uint64, error) { return 8, nil }, visit)
	if err != nil || end != 0 {
		t.Fatal(end, err)
	}
	w, err := OpenWAL(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.RepairTail(end); err != nil {
		t.Fatal(err)
	}
	if size, _ := w.Offset(); size != 0 {
		t.Fatal(size)
	}
}
