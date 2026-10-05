package simhash

import (
	"unsafe"

	hashkernel "github.com/evanoberholster/imageIdx/index/simhash/internal/hashkernel"
)

// hashWords reinterprets a PDQHash as four host-endian uint64 words.
func hashWords(h *PDQHash) *[4]uint64 { return (*[4]uint64)(unsafe.Pointer(h)) }

// DistancePHash returns the exact 64-bit Hamming distance between two pHashes.
func DistancePHash(a, b uint64) int { return hashkernel.PHashDistance(a, b) }

// DistancePDQ returns the exact 256-bit Hamming distance between two PDQ
// hashes. On arm64 this is the NEON kernel.
func DistancePDQ(a, b *PDQHash) uint16 {
	return hashkernel.PDQDistance((*[32]byte)(unsafe.Pointer(a)), (*[32]byte)(unsafe.Pointer(b)))
}
