// Package roar implements an immutable, offset-based bitmap index over
// database-wide uint64 image IDs. The serialized byte region is the read
// representation: a Bitmap operates directly over mmap-backed bytes without
// decoding container objects.
//
// The serialized format is frozen at version 1; see FORMAT.md. Any change
// that alters the layout must bump Version rather than modify this format.
//
// IDs are split into an upper 48-bit container key and a lower 16-bit value.
// Containers are either sorted uint16 arrays (sparse) or 1,024 uint64 words
// (dense). ImageID zero is reserved and never represents an image.
//
// The format is little-endian by design: all multi-byte fields are encoded
// with the host's native little-endian order and read back with zero-copy
// unsafe casts, trading portability for speed and compactness. Open rejects
// big-endian hosts (ErrUnsupportedEndian); constructors and OpenTrusted
// assume a little-endian host as a precondition of the format.
package roar

import (
	"errors"
	"unsafe"
)

const (
	// Version is the version-1 format tag. roar bitmaps are always embedded
	// inside parent envelopes that carry their own magic, so the bitmap
	// header stores no signature of its own.
	Version uint16 = 1

	// Alignment is the required byte alignment of every structural boundary
	// in the serialized format. The header, key table, container headers,
	// and bitmap payloads all begin on Alignment-byte boundaries, and every
	// container occupies a multiple of Alignment bytes. This keeps zero-copy
	// uint64 access valid and leaves room for future SIMD alignment.
	Alignment = 8

	HeaderSize          = 16
	KeyEntrySize        = 16
	ContainerHeaderSize = 8

	BitmapWords = 1024
	BitmapBytes = BitmapWords * 8

	// ArrayToBitmapThreshold is the initial container conversion threshold.
	// Arrays at or below this cardinality stay arrays; builders promote above
	// it. The value is a runtime policy, not a format guarantee.
	ArrayToBitmapThreshold = 2048
)

const (
	FlagCompact uint16 = 1 << iota
	FlagStoredCardinality
)

// InvalidImageID is reserved and never represents an image.
const InvalidImageID uint64 = 0

var (
	ErrInvalidBitmap         = errors.New("roar: invalid bitmap")
	ErrUnsupportedVersion    = errors.New("roar: unsupported version")
	ErrInvalidImageID        = errors.New("roar: invalid image id")
	ErrCorruptContainer      = errors.New("roar: corrupt container")
	ErrUnsupportedEndian     = errors.New("roar: serialized format requires a little-endian host")
	ErrUnsortedIDs           = errors.New("roar: image ids must be strictly increasing")
	ErrSortedBuilderFinished = errors.New("roar: sorted builder is finished; call Reset before Add")
	ErrSelectOutOfRange      = errors.New("roar: select index out of range")
)

// ContainerType identifies the payload kind stored after the container
// header.
type ContainerType uint16

const (
	ContainerArray ContainerType = iota
	ContainerBitmap
)

// header is the fixed 16-byte leading record. The key table always starts
// immediately after it, so no offset field is stored; every field sits at
// its natural alignment with no padding.
type header struct {
	Version     uint16
	Flags       uint16
	KeyCount    uint32
	Cardinality uint64
}

// keyEntry is one fixed 16-byte record mapping an upper-48-bit key to the
// byte offset of its container.
type keyEntry struct {
	Key    uint64
	Offset uint64
}

// containerHeader precedes every container. Blocks stores the allocated
// container size in 8-byte blocks, keeping payloads naturally aligned.
type containerHeader struct {
	Blocks uint16
	Type   uint16
	Card   uint32
}

const lowMask uint64 = 0xffff

func highKey(id uint64) uint64 { return id &^ lowMask }

func lowValue(id uint64) uint16 { return uint16(id) }

// requireAligned panics if data is non-empty and its base address is not
// Alignment-byte aligned. Every constructor that produces a fresh serialized
// region and hands it to zero-copy uint64 access must call it.
//
// Go only guarantees single-byte alignment for make([]byte, n) and
// bytes.Clone; in practice the runtime returns 8-byte-aligned pointers on
// 64-bit little-endian targets, but the guarantee is not in the language
// spec. This turns any platform where that assumption breaks into an
// immediate, loud failure instead of silent memory corruption from unaligned
// unsafe reads.
func requireAligned(data []byte) {
	if len(data) == 0 {
		return
	}
	if uintptr(unsafe.Pointer(unsafe.SliceData(data)))%Alignment != 0 {
		panic("roar: constructed buffer is not 8-byte aligned")
	}
}
