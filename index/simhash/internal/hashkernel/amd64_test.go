//go:build amd64

package hashkernel

import (
	"math/rand"
	"testing"
)

// TestAVX2PDQBlockMaskAgainstScalar validates the AVX2 kernel against the scalar
// reference. It runs on amd64 hosts; on others the file is excluded.
func TestAVX2PDQBlockMaskAgainstScalar(t *testing.T) {
	if !avx2Available {
		t.Skip("AVX2 not available")
	}
	rng := rand.New(rand.NewSource(4001))
	for iter := 0; iter < 3000; iter++ {
		n := rng.Intn(BlockRecords) + 1
		base := make([]byte, n*32)
		for i := range base {
			base[i] = byte(rng.Intn(256))
		}
		var q [32]byte
		if iter%2 == 0 {
			copy(q[:], base[rng.Intn(n)*32:])
			for k := 0; k < rng.Intn(20); k++ {
				q[rng.Intn(32)] ^= 1 << uint(rng.Intn(8))
			}
		} else {
			for j := range q {
				q[j] = byte(rng.Intn(256))
			}
		}
		limit := rng.Intn(50)
		got := avx2PDQBlockMask(&q, base, n, limit)
		want := Scalar.PDQBlockMask(&q, base, n, limit)
		if got != want {
			t.Fatalf("iter %d n %d limit %d: avx2 %#x scalar %#x", iter, n, limit, got, want)
		}
	}
}

func BenchmarkAVX2PDQBlockMask(b *testing.B) {
	q, base := benchBlocks(3)
	if !avx2Available {
		b.Skip("AVX2 not available")
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = avx2PDQBlockMask(q, base, BlockRecords, 16)
	}
}

func BenchmarkScalarPDQBlockMaskAMD64(b *testing.B) {
	q, base := benchBlocks(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Scalar.PDQBlockMask(q, base, BlockRecords, 16)
	}
}
