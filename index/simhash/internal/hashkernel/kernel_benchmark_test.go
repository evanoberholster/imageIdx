package hashkernel

import (
	"math/rand"
	"testing"
	"unsafe"
)

func benchBlocks(n int) (*[32]byte, []byte) {
	rng := rand.New(rand.NewSource(99))
	base := make([]byte, BlockRecords*32)
	for i := range base {
		base[i] = byte(rng.Intn(256))
	}
	var q [32]byte
	copy(q[:], base[(n%BlockRecords)*32:])
	for k := 0; k < 12; k++ {
		q[rng.Intn(32)] ^= 1 << uint(rng.Intn(8))
	}
	return &q, base
}

func BenchmarkPDQDistanceScalar(b *testing.B) {
	q, base := benchBlocks(0)
	h := (*[32]byte)(unsafe.Pointer(&base[0]))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Scalar.PDQDistance(q, h)
	}
}

func BenchmarkPDQDistanceCurrent(b *testing.B) {
	q, base := benchBlocks(0)
	h := (*[32]byte)(unsafe.Pointer(&base[0]))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Current.PDQDistance(q, h)
	}
}

func BenchmarkPHashScalar(b *testing.B) {
	rng := rand.New(rand.NewSource(5))
	q := rng.Uint64()
	hashes := make([]uint64, BlockRecords)
	for i := range hashes {
		hashes[i] = rng.Uint64()
	}
	var sink uint64
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = 0
		for j := range hashes {
			sink += uint64(Scalar.PHashDistance(q, hashes[j]))
		}
	}
	_ = sink
}

func BenchmarkPDQBlockMaskScalar(b *testing.B) {
	q, base := benchBlocks(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Scalar.PDQBlockMask(q, base, BlockRecords, 16)
	}
}

func BenchmarkPDQBlockMaskCurrent(b *testing.B) {
	q, base := benchBlocks(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Current.PDQBlockMask(q, base, BlockRecords, 16)
	}
}
