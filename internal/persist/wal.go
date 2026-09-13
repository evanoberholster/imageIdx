package persist

import (
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// WAL is framed write-ahead-log transport: open, offset, close, reusable
// scratch, and shared header/payload framing with rollback and
// truncated-tail-tolerant replay. Record headers, payload codecs, magic
// numbers, and corruption errors stay in the owning index package.
type WAL struct {
	file *os.File
	buf  []byte
}

// OpenWAL opens (creating) the WAL at path for appends. bufCap preallocates
// reusable scratch returned by Scratch.
func OpenWAL(path string, bufCap int) (*WAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &WAL{file: file, buf: make([]byte, 0, bufCap)}, nil
}

// Offset returns the current end-of-file position.
func (w *WAL) Offset() (int64, error) { return w.file.Seek(0, io.SeekEnd) }

// Close closes the WAL file.
func (w *WAL) Close() error { return w.file.Close() }

// Truncate discards all WAL contents. Callers must first publish a checkpoint
// or manifest that no longer depends on the discarded records, and must
// serialize against concurrent appends.
func (w *WAL) Truncate() error {
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	_, err := w.file.Seek(0, io.SeekStart)
	return err
}

// Scratch returns reusable scratch of length n, growing the WAL buffer. The
// slice is valid until the next Scratch call; only one Scratch slice may be
// live at a time, and callers must fully encode it.
func (w *WAL) Scratch(n int) []byte {
	if cap(w.buf) < n {
		w.buf = make([]byte, n)
	} else {
		w.buf = w.buf[:n]
	}
	return w.buf
}

// AppendFrame writes header followed by payload, truncating back to the
// starting offset when a write fails, then optionally fsyncing.
func (w *WAL) AppendFrame(header, payload []byte, sync bool) error {
	start, err := w.Offset()
	if err != nil {
		return err
	}
	rollback := func(writeErr error) error {
		_ = w.file.Truncate(start)
		_, _ = w.file.Seek(0, io.SeekEnd)
		return writeErr
	}
	if err := indexutil.WriteFull(w.file, header); err != nil {
		return rollback(err)
	}
	if err := indexutil.WriteFull(w.file, payload); err != nil {
		return rollback(err)
	}
	if sync {
		return w.file.Sync()
	}
	return nil
}

// WriteRecord writes one contiguous record with a single write, preserving
// short-write detection, then optionally fsyncing.
func (w *WAL) WriteRecord(record []byte, sync bool) error {
	written, err := w.file.Write(record)
	if err != nil {
		return err
	}
	if written != len(record) {
		return io.ErrShortWrite
	}
	if sync {
		return w.file.Sync()
	}
	return nil
}

// FrameSpec describes the framing shared by every index WAL format: magic
// and version occupy header[0:4] and header[4:6]; Corrupt is the owner's
// corruption error returned for framing violations.
type FrameSpec struct {
	HeaderSize int
	Magic      uint32
	Version    uint16
	Corrupt    error
}

// Replay walks framed records from offset, resolving each header's payload
// length with payloadLen and decoding each record with visit. A missing file
// and truncated header/payload tails are clean stops, not errors.
func Replay(path string, offset int64, spec FrameSpec, payloadLen func(header []byte) (uint64, error), visit func(header, payload []byte) error) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	header := make([]byte, spec.HeaderSize)
	for {
		if _, err = io.ReadFull(file, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != spec.Magic ||
			binary.LittleEndian.Uint16(header[4:6]) != spec.Version {
			return spec.Corrupt
		}
		size, err := payloadLen(header)
		if err != nil {
			return err
		}
		payload := make([]byte, size)
		if _, err = io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if err = visit(header, payload); err != nil {
			return err
		}
	}
}
