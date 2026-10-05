package simhash

import (
	"math/bits"
	"unsafe"
)

// hashWords reinterprets a PDQHash as four host-endian uint64 words.
func hashWords(h *PDQHash) *[4]uint64 { return (*[4]uint64)(unsafe.Pointer(h)) }

// DistancePHash returns the exact 64-bit Hamming distance between two pHashes.
func DistancePHash(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// DistancePDQ returns the exact 256-bit Hamming distance between two PDQ
// hashes.
func DistancePDQ(a, b *PDQHash) uint16 {
	aw := hashWords(a)
	bw := hashWords(b)
	return uint16(bits.OnesCount64(aw[0]^bw[0]) +
		bits.OnesCount64(aw[1]^bw[1]) +
		bits.OnesCount64(aw[2]^bw[2]) +
		bits.OnesCount64(aw[3]^bw[3]))
}
