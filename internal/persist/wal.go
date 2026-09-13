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
	file   walFile
	buf    []byte
	failed error
}

type walFile interface {
	io.Writer
	io.Seeker
	Truncate(int64) error
	Sync() error
	Close() error
}

// ErrWALFailed means the writer must be closed and recovered before reuse.
var ErrWALFailed = errors.New("persist: WAL requires recovery")

// Sync establishes a durable boundary, refusing a previously failed writer.
func (w *WAL) Sync() error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.file.Sync(); err != nil {
		return w.fail(err)
	}
	return nil
}

func (w *WAL) fail(err error) error {
	w.failed = errors.Join(ErrWALFailed, err)
	return w.failed
}

// RepairTail removes only a tail already verified incomplete by replay.
// The caller must exclusively own the WAL and must not yet accept writes.
func (w *WAL) RepairTail(end int64) error {
	if w.failed != nil {
		return w.failed
	}
	size, err := w.Offset()
	if err != nil {
		return w.fail(err)
	}
	if end < 0 || end > size {
		return w.fail(errors.New("persist: invalid repair boundary"))
	}
	if end == size {
		return nil
	}
	if err := w.file.Truncate(end); err != nil {
		return w.fail(err)
	}
	return w.Sync()
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
	if w.failed != nil {
		return w.failed
	}
	start, err := w.Offset()
	if err != nil {
		return w.fail(err)
	}
	rollback := func(writeErr error) error {
		truncateErr := w.file.Truncate(start)
		_, seekErr := w.file.Seek(0, io.SeekEnd)
		return w.fail(errors.Join(writeErr, truncateErr, seekErr))
	}
	if err := indexutil.WriteFull(w.file, header); err != nil {
		return rollback(err)
	}
	if err := indexutil.WriteFull(w.file, payload); err != nil {
		return rollback(err)
	}
	if sync {
		return w.Sync()
	}
	return nil
}

// WriteRecord writes one contiguous record with a single write, preserving
// short-write detection, then optionally fsyncing.
func (w *WAL) WriteRecord(record []byte, sync bool) error {
	return w.AppendFrame(record, nil, sync)
}

// FrameSpec describes the framing shared by every index WAL format: magic
// and version occupy header[0:4] and header[4:6]; Corrupt is the owner's
// corruption error returned for framing violations.
type FrameSpec struct {
	HeaderSize int
	Magic      uint32
	Version    uint16
	Corrupt    error
	MaxPayload uint64 // zero leaves the transport limit unspecified
}

// Replay walks framed records from offset, resolving each header's payload
// length with payloadLen and decoding each record with visit. A missing file
// and truncated header/payload tails are clean stops, not errors.
func Replay(path string, offset int64, spec FrameSpec, payloadLen func(header []byte) (uint64, error), visit func(header, payload []byte) error) error {
	_, err := ReplayEnd(path, offset, spec, payloadLen, visit)
	return err
}

// ReplayEnd is Replay plus the exclusive end of the last complete frame.
// It never repairs a file; the exclusive writer may subsequently RepairTail.
func ReplayEnd(path string, offset int64, spec FrameSpec, payloadLen func(header []byte) (uint64, error), visit func(header, payload []byte) error) (int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return offset, nil
	}
	if err != nil {
		return offset, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return offset, err
	}
	if offset < 0 || offset > info.Size() || spec.HeaderSize < 6 {
		return offset, spec.Corrupt
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	header := make([]byte, spec.HeaderSize)
	for {
		if _, err = io.ReadFull(file, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return offset, nil
			}
			return offset, err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != spec.Magic ||
			binary.LittleEndian.Uint16(header[4:6]) != spec.Version {
			return offset, spec.Corrupt
		}
		size, err := payloadLen(header)
		if err != nil {
			return offset, err
		}
		if size > uint64(^uint(0)>>1) || (spec.MaxPayload != 0 && size > spec.MaxPayload) {
			return offset, spec.Corrupt
		}
		if size > uint64(info.Size()-offset)-uint64(spec.HeaderSize) {
			return offset, nil
		}
		payload := make([]byte, size)
		if _, err = io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return offset, nil
			}
			return offset, err
		}
		if err = visit(header, payload); err != nil {
			return offset, err
		}
		offset += int64(spec.HeaderSize) + int64(size)
	}
}
