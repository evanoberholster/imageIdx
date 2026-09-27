package storage

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// ErrCorruptManifest is returned by VerifyManifest; index packages translate
// it to their own corruption error at the call site.
var ErrCorruptManifest = errors.New("storage: corrupt manifest")

// BuildManifest allocates size bytes, stamps magic/version, lets fill encode
// the remaining fields, then stores crc32 over data[:crcCovered] at
// data[crcAt:crcAt+4].
func BuildManifest(size int, magic uint32, version uint16, crcCovered, crcAt int, fill func(data []byte)) []byte {
	data := make([]byte, size)
	binary.LittleEndian.PutUint32(data[0:4], magic)
	binary.LittleEndian.PutUint16(data[4:6], version)
	fill(data)
	binary.LittleEndian.PutUint32(data[crcAt:crcAt+4], crc32.ChecksumIEEE(data[:crcCovered]))
	return data
}

// PeekHeader splits magic and version from framed bytes without
// validating anything else. ok is false when fewer than 6 bytes are
// present. Callers compare against their expected magic and version to
// tell newer formats (unsupported) apart from corruption.
func PeekHeader(data []byte) (magic uint32, version uint16, ok bool) {
	if len(data) < 6 {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint32(data[0:4]), binary.LittleEndian.Uint16(data[4:6]), true
}

// VerifyManifest checks size, magic, version, and the trailing checksum.
func VerifyManifest(data []byte, size int, magic uint32, version uint16, crcCovered, crcAt int) error {
	if len(data) != size || binary.LittleEndian.Uint32(data[0:4]) != magic ||
		binary.LittleEndian.Uint16(data[4:6]) != version ||
		binary.LittleEndian.Uint32(data[crcAt:crcAt+4]) != crc32.ChecksumIEEE(data[:crcCovered]) {
		return ErrCorruptManifest
	}
	return nil
}

// WriteManifest atomically publishes data as dir/manifest.
func WriteManifest(dir string, data []byte) error {
	return WriteFileAtomic(dir, "manifest.tmp", "manifest", data, 0o644)
}
