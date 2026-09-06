// Package roar implements an immutable, offset-based bitmap index over
// database-wide uint64 image IDs. The serialized byte region is the read
// representation: a Bitmap operates directly over mmap-backed bytes without
// decoding container objects.
//
// IDs are split into an upper 48-bit container key and a lower 16-bit value.
// Containers are either sorted uint16 arrays (sparse) or 1,024 uint64 words
// (dense). ImageID zero is reserved and never represents an image.
package roar

import "errors"

const (
	// Magic is the version-1 byte signature "ROAR".
	Magic   uint32 = 0x52414F52
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

// legacyMagic is the transposed "RAOR" signature emitted before Magic was
// corrected. Open accepts it so existing version-1 images remain readable;
// constructors always emit Magic.
const legacyMagic uint32 = 0x524F4152

const (
	FlagCompact uint16 = 1 << iota
	FlagStoredCardinality
)

// InvalidImageID is reserved and never represents an image.
const InvalidImageID uint64 = 0

var (
	ErrInvalidBitmap         = errors.New("roar: invalid bitmap")
	ErrInvalidMagic          = errors.New("roar: invalid magic")
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

// header is the fixed 16-byte leading record.
type header struct {
	Magic       uint32
	Version     uint16
	Flags       uint16
	HeaderBytes uint32
	KeyCount    uint32
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
