package roar

import (
	"encoding/binary"
	"math/bits"
	"unsafe"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// Open validates data and returns an immutable View over it. Validation
// covers the header, key table, container bounds, container types, sorted
// array payloads, bitmap payload cardinalities, and the alignment required
// by zero-copy access.
func Open(data []byte) (View, error) {
	if err := Validate(data); err != nil {
		return View{}, err
	}
	return View{data: data}, nil
}

// Validate checks that data forms a well-formed version-1 bitmap. It uses
// the portable safe decoder and performs no unchecked typed access, so it is
// safe for untrusted input.
func Validate(data []byte) error {
	if len(data) < HeaderSize {
		return ErrInvalidBitmap
	}
	if binary.LittleEndian.Uint32(data[0:4]) != Magic {
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
		if offset < tableEnd || offset%Alignment != 0 || offset+ContainerHeaderSize > uint64(len(data)) {
			return ErrInvalidBitmap
		}
		if err := validateContainer(data[offset:]); err != nil {
			return err
		}
	}
	return nil
}

func validateContainer(container []byte) error {
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
