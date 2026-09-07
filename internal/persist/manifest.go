package persist

import (
	"encoding/binary"
	"errors"
	"hash/crc32"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// ErrCorruptManifest is returned by VerifyManifest; index packages translate
// it to their own corruption error at the call site.
var ErrCorruptManifest = errors.New("persist: corrupt manifest")

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
	return indexutil.WriteFileAtomic(dir, "manifest.tmp", "manifest", data, 0o644)
}

// WriteManifestOnce is WriteManifest for callers whose publication path uses
// a single write operation.
func WriteManifestOnce(dir string, data []byte) error {
	return indexutil.WriteFileAtomicOnce(dir, "manifest.tmp", "manifest", data, 0o644)
}
