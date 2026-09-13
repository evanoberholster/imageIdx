package roar

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"unsafe"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// MarshalBinary returns an owned copy of the serialized representation. The
// returned bytes are independent of the Bitmap's backing storage, so callers
// may persist or hand them off while the original mmap is retained or freed.
// It implements encoding.BinaryMarshaler.
func (b Bitmap) MarshalBinary() ([]byte, error) {
	if len(b.data) == 0 {
		return assemble(nil, nil, nil, 0), nil
	}
	cloned := bytes.Clone(b.data)
	requireAligned(cloned)
	return cloned, nil
}

// UnmarshalBinary validates data and replaces the receiver with a Bitmap over
// it. It implements encoding.BinaryUnmarshaler.
func (b *Bitmap) UnmarshalBinary(data []byte) error {
	parsed, err := Open(data)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

// Clone returns a Bitmap over an owned copy of the backing bytes. It is
// useful for decoupling a Bitmap from an mmap or a shared buffer.
func (b Bitmap) Clone() Bitmap {
	if len(b.data) == 0 {
		return Bitmap{}
	}
	cloned := bytes.Clone(b.data)
	requireAligned(cloned)
	return Bitmap{data: cloned}
}

// CloneToBuf returns a Bitmap over an owned copy of the backing bytes,
// reusing reuse's backing array when it has enough capacity. The returned
// Bitmap is independent of the receiver and of later reuse of the buffer.
func (b Bitmap) CloneToBuf(reuse []byte) Bitmap {
	if len(b.data) == 0 {
		return Bitmap{}
	}
	if cap(reuse) >= len(b.data) {
		out := reuse[:len(b.data)]
		copy(out, b.data)
		requireAligned(out)
		return Bitmap{data: out}
	}
	return b.Clone()
}

// Bytes returns the underlying serialized bytes without copying. The caller
// must not modify the returned slice or free the backing storage while the
// Bitmap is in use.
func (b Bitmap) Bytes() []byte {
	return b.data
}

// LenInBytes returns the length in bytes of the serialized representation.
// It matches the length of the bytes returned by MarshalBinary, including the
// 24-byte header for an empty bitmap. It is the on-disk size of the bitmap.
func (b Bitmap) LenInBytes() int {
	if len(b.data) == 0 {
		return HeaderSize
	}
	return len(b.data)
}

// Open validates data and returns an immutable Bitmap over it. Validation
// covers the header, key table, container bounds, container types, sorted
// array payloads, bitmap payload cardinalities, and the alignment required
// by zero-copy access.
func Open(data []byte) (Bitmap, error) {
	if err := Validate(data); err != nil {
		return Bitmap{}, err
	}
	return Bitmap{data: data}, nil
}

// Validate checks that data forms a well-formed version-1 bitmap. It uses
// the portable safe decoder and performs no unchecked typed access, so it is
// safe for untrusted input.
func Validate(data []byte) error {
	if len(data) < HeaderSize {
		return ErrInvalidBitmap
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != Magic {
		return ErrInvalidMagic
	}
	if binary.LittleEndian.Uint16(data[4:6]) != Version {
		return ErrUnsupportedVersion
	}
	if !indexutil.HostIsLittleEndian() {
		return ErrUnsupportedEndian
	}
	headerBytes := binary.LittleEndian.Uint32(data[8:12])
	keyCount := binary.LittleEndian.Uint32(data[12:16])
	flags := binary.LittleEndian.Uint16(data[6:8])
	if headerBytes < HeaderSize || uint64(headerBytes) > uint64(len(data)) ||
		headerBytes%Alignment != 0 || uintptr(unsafe.Pointer(unsafe.SliceData(data)))%Alignment != 0 {
		return ErrInvalidBitmap
	}
	tableStart := uint64(headerBytes)
	tableEnd, ok := indexutil.CheckedEnd(tableStart, uint64(keyCount), KeyEntrySize, uint64(len(data)))
	if !ok {
		return ErrInvalidBitmap
	}

	var previous uint64
	var totalCard uint64
	for index := uint32(0); index < keyCount; index++ {
		entry := tableStart + uint64(index)*KeyEntrySize
		key := binary.LittleEndian.Uint64(data[entry : entry+8])
		offset := binary.LittleEndian.Uint64(data[entry+8 : entry+16])
		if key&lowMask != 0 {
			return ErrCorruptContainer
		}
		if index != 0 && key <= previous {
			return ErrInvalidBitmap
		}
		previous = key
		if offset < tableEnd || offset%Alignment != 0 {
			return ErrInvalidBitmap
		}
		if _, ok := indexutil.CheckedEnd(offset, 1, ContainerHeaderSize, uint64(len(data))); !ok {
			return ErrInvalidBitmap
		}
		if err := validateContainer(data[offset:], key); err != nil {
			return err
		}
		totalCard += uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
	}
	if flags&FlagStoredCardinality != 0 {
		if binary.LittleEndian.Uint64(data[16:24]) != totalCard {
			return ErrCorruptContainer
		}
	}
	return nil
}

func validateContainer(container []byte, key uint64) error {
	if len(container) < ContainerHeaderSize {
		return ErrCorruptContainer
	}
	blocks := binary.LittleEndian.Uint16(container[0:2])
	containerType := ContainerType(binary.LittleEndian.Uint16(container[2:4]))
	card := binary.LittleEndian.Uint32(container[4:8])
	end := uint64(blocks) * 8
	if blocks < 2 || end > uint64(len(container)) || card == 0 {
		return ErrCorruptContainer
	}
	switch containerType {
	case ContainerArray:
		slots := (end - ContainerHeaderSize) / 2
		if uint64(card) > slots {
			return ErrCorruptContainer
		}
		var previous uint16
		for index := uint32(0); index < card; index++ {
			value := binary.LittleEndian.Uint16(container[ContainerHeaderSize+uint64(index)*2:])
			if key == 0 && value == 0 {
				return ErrInvalidImageID
			}
			if index != 0 && value <= previous {
				return ErrCorruptContainer
			}
			previous = value
		}
	case ContainerBitmap:
		if blocks != 1+BitmapWords || card > 1<<16 {
			return ErrCorruptContainer
		}
		var count uint32
		for wordIndex := 0; wordIndex < BitmapWords; wordIndex++ {
			word := binary.LittleEndian.Uint64(container[ContainerHeaderSize+uint64(wordIndex)*8:])
			if key == 0 && wordIndex == 0 && word&1 != 0 {
				return ErrInvalidImageID
			}
			count += uint32(bits.OnesCount64(word))
		}
		if count != card {
			return ErrCorruptContainer
		}
	default:
		return ErrCorruptContainer
	}
	return nil
}
