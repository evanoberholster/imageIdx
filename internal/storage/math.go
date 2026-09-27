package storage

// SaturatingAdd returns left+right, capped at the largest uint64 value.
func SaturatingAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}
