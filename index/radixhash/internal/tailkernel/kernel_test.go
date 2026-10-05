package tailkernel

import (
	"bytes"
	"math/rand"
	"testing"
)

func fillTails(rng *rand.Rand, count, stride int, want []byte, hits []int) []byte {
	tails := make([]byte, count*stride)
	for i := range tails {
		tails[i] = byte(rng.Intn(256))
	}
	for _, h := range hits {
		copy(tails[h*stride:h*stride+16], want[:16])
	}
	return tails
}

func expectedMask(tails []byte, stride, count int, want []byte) uint64 {
	var mask uint64
	for i := 0; i < count; i++ {
		if bytes.Equal(tails[i*stride:i*stride+16], want[:16]) {
			mask |= uint64(1) << uint(i)
		}
	}
	return mask
}

func TestMatchMaskAgainstScalar(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	want := make([]byte, 30)
	for i := range want {
		want[i] = byte(rng.Intn(256))
	}
	for _, count := range []int{1, 2, 3, 8, 16, 32, 64} {
		hits := []int{0}
		if count > 4 {
			hits = append(hits, count-1)
		}
		if count > 8 {
			hits = append(hits, count/2)
		}
		tails := fillTails(rng, count, 30, want, hits)
		wantMask := expectedMask(tails, 30, count, want)
		if got := MatchMask(tails, 30, count, want); got != wantMask {
			t.Fatalf("count=%d mask=%064b want %064b", count, got, wantMask)
		}
	}
}

func TestMatchMaskPrefixOnly(t *testing.T) {
	want := make([]byte, 30)
	for i := range want {
		want[i] = byte(i)
	}
	tails := make([]byte, 2*30)
	copy(tails[0:16], want[:16])
	copy(tails[30:46], want[:16])
	// Slot 0's remaining bytes differ from want; the 16-byte prefix still matches.
	for i := 16; i < 30; i++ {
		tails[i] = 0xff
	}
	if got := MatchMask(tails, 30, 2, want); got != 0b11 {
		t.Fatalf("mask=%064b, want 0b11", got)
	}
}

func TestMatchMaskBounds(t *testing.T) {
	want := make([]byte, 16)
	tails := make([]byte, 64*30)
	if MatchMask(tails, 15, 4, want) != 0 {
		t.Fatal("stride < 16 should return zero")
	}
	if MatchMask(tails, 30, 4, want[:15]) != 0 {
		t.Fatal("short want should return zero")
	}
	if MatchMask(tails[:31], 30, 4, want) != 0 {
		t.Fatal("short tails should return zero")
	}
	if got := MatchMask(tails, 30, 100, want); got != ^uint64(0) {
		t.Fatalf("clamped mask=%064b, want all set", got)
	}
}
