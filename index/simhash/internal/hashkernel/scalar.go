// Package hashkernel provides the scalar and architecture-specific Hamming
// distance kernels behind one dispatch point.
//
// The scalar reference (math/bits) is the correctness oracle and the
// concurrent-safe implementation: it can be called on values loaded with
// sync/atomic. The architecture kernels (NEON on arm64; AVX2 on amd64) read
// plain memory with wide loads and are therefore only safe on quiescent data — a
// scan that cannot run concurrently with an in-place writer, or immutable
// append-only data.
//
// A PDQ hash is 32 raw bytes; a pHash is 8. The block kernels produce a 64-bit
// mask whose bit i is set when record i (32 bytes apart) is within the limit.
package hashkernel

import (
	"math/bits"
	"unsafe"
)

// BlockRecords is the maximum number of records a block kernel processes per
// call.
const BlockRecords = 64

// Funcs is one coherent set of kernels.
type Funcs struct {
	// PHashDistance returns the exact 64-bit Hamming distance.
	PHashDistance func(a, b uint64) int
	// PDQDistance returns the exact 256-bit Hamming distance.
	PDQDistance func(a, b *[32]byte) uint16
	// PDQBlockMask returns a mask for up to BlockRecords 32-byte records
	// starting at base; bit i is set when record i is within limit.
	PDQBlockMask func(q *[32]byte, base []byte, count, limit int) uint64
}

// Scalar is the portable reference implementation.
var Scalar = Funcs{
	PHashDistance: scalarPHashDistance,
	PDQDistance:   scalarPDQDistance,
	PDQBlockMask:  scalarPDQBlockMask,
}

// Current is the active kernel set. It starts as Scalar and is replaced by an
// architecture backend at init when one is available.
var Current = Scalar

// words reinterprets a 32-byte hash as four host-endian uint64 words. The
// interpretation is process-local; distance is order-independent.
func words(p *[32]byte) *[4]uint64 { return (*[4]uint64)(unsafe.Pointer(p)) }

func scalarPHashDistance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

func scalarPDQDistance(a, b *[32]byte) uint16 {
	aw := words(a)
	bw := words(b)
	return uint16(bits.OnesCount64(aw[0]^bw[0]) +
		bits.OnesCount64(aw[1]^bw[1]) +
		bits.OnesCount64(aw[2]^bw[2]) +
		bits.OnesCount64(aw[3]^bw[3]))
}

func scalarPDQBlockMask(q *[32]byte, base []byte, count, limit int) uint64 {
	if count > BlockRecords {
		count = BlockRecords
	}
	qw := words(q)
	var mask uint64
	for i := 0; i < count; i++ {
		hw := (*[4]uint64)(unsafe.Pointer(&base[i*32]))
		d := bits.OnesCount64(qw[0]^hw[0]) +
			bits.OnesCount64(qw[1]^hw[1]) +
			bits.OnesCount64(qw[2]^hw[2]) +
			bits.OnesCount64(qw[3]^hw[3])
		if int(d) <= limit {
			mask |= 1 << uint(i)
		}
	}
	return mask
}
