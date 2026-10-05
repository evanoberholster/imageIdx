package hashkernel

import (
	"math/bits"
	"unsafe"
)

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
