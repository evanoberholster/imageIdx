package tailkernel

import "bytes"

// matchMaskScalar is the portable reference implementation.
func matchMaskScalar(tails []byte, stride, count int, want []byte) uint64 {
	prefix := want[:16]
	var mask uint64
	for i := 0; i < count; i++ {
		if bytes.Equal(tails[i*stride:i*stride+16], prefix) {
			mask |= uint64(1) << uint(i)
		}
	}
	return mask
}
