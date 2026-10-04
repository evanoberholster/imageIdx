package tailkernel

import (
	"fmt"
	"testing"
)

func benchmarkMatchMask(b *testing.B, kernel func([]byte, int, int, []byte) uint64, count int) {
	tails := make([]byte, count*30)
	want := make([]byte, 30)
	// Put one hit at the end so every slot is examined.
	copy(tails[(count-1)*30:], want)
	b.ResetTimer()
	var sink uint64
	for i := 0; i < b.N; i++ {
		sink ^= kernel(tails, 30, count, want)
	}
	_ = sink
}

func BenchmarkMatchMaskScalar(b *testing.B) {
	for _, count := range []int{4, 8, 16, 32, 64} {
		b.Run(fmt.Sprintf("n=%d", count), func(b *testing.B) {
			benchmarkMatchMask(b, matchMaskScalar, count)
		})
	}
}

func BenchmarkMatchMaskBackend(b *testing.B) {
	for _, count := range []int{4, 8, 16, 32, 64} {
		b.Run(fmt.Sprintf("n=%d", count), func(b *testing.B) {
			benchmarkMatchMask(b, MatchMask, count)
		})
	}
}
