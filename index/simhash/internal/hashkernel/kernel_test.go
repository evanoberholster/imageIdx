package hashkernel

import (
	"math/rand"
	"testing"
	"unsafe"
)

func TestPHashDistanceAgainstScalar(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		a, b := rng.Uint64(), rng.Uint64()
		if got, want := Current.PHashDistance(a, b), Scalar.PHashDistance(a, b); got != want {
			t.Fatalf("PHashDistance = %d, want %d", got, want)
		}
	}
}

func TestPDQDistanceAgainstScalar(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 20000; i++ {
		var a, b [32]byte
		for j := range a {
			a[j] = byte(rng.Intn(256))
			b[j] = byte(rng.Intn(256))
		}
		if got, want := Current.PDQDistance(&a, &b), Scalar.PDQDistance(&a, &b); got != want {
			t.Fatalf("PDQDistance = %d, want %d", got, want)
		}
	}
}

// TestPDQDistanceLadder compares every implementation at the exact Hamming
// distances used by the correctness suite.
func TestPDQDistanceLadder(t *testing.T) {
	var base [32]byte
	for i := range base {
		base[i] = byte(0xA5 ^ (i * 7))
	}
	rng := rand.New(rand.NewSource(7))
	for _, d := range []int{0, 1, 2, 7, 8, 15, 16, 31, 32, 63, 64, 127, 128, 255, 256} {
		q := base
		order := rng.Perm(256)
		for _, bit := range order[:d] {
			q[bit/8] ^= 1 << uint(bit%8)
		}
		if got, want := Current.PDQDistance(&q, &base), Scalar.PDQDistance(&q, &base); got != want || int(got) != d {
			t.Fatalf("d=%d: Current=%d Scalar=%d", d, got, want)
		}
		// Threshold inclusion at d-1, d, d+1.
		for _, limit := range []int{d - 1, d, d + 1} {
			if limit < 0 {
				continue
			}
			got := Current.PDQBlockMask(&q, base[:], 1, limit)
			want := Scalar.PDQBlockMask(&q, base[:], 1, limit)
			if got != want || (got&1 == 1) != (d <= limit) {
				t.Fatalf("d=%d limit=%d: Current=%#x Scalar=%#x", d, limit, got, want)
			}
		}
	}
}

func TestPDQBlockMaskAgainstScalar(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	limits := []int{0, 1, 4, 8, 16, 31, 32, 40, 64, 256}
	for iter := 0; iter < 2000; iter++ {
		n := rng.Intn(BlockRecords) + 1
		base := make([]byte, n*32)
		for i := range base {
			base[i] = byte(rng.Intn(256))
		}
		var q [32]byte
		// Half the time derive the query from a stored record so matches occur.
		if iter%2 == 0 {
			copy(q[:], base[(rng.Intn(n))*32:])
			for k := 0; k < rng.Intn(20); k++ {
				q[rng.Intn(32)] ^= 1 << uint(rng.Intn(8))
			}
		} else {
			for j := range q {
				q[j] = byte(rng.Intn(256))
			}
		}
		limit := limits[iter%len(limits)]
		got := Current.PDQBlockMask(&q, base, n, limit)
		want := Scalar.PDQBlockMask(&q, base, n, limit)
		if got != want {
			t.Fatalf("iter %d n %d limit %d: got %#x want %#x", iter, n, limit, got, want)
		}
		// Verify bit semantics independently.
		for i := 0; i < n; i++ {
			d := Scalar.PDQDistance(&q, (*[32]byte)(unsafe.Pointer(&base[i*32])))
			set := got>>uint(i)&1 == 1
			if set != (int(d) <= limit) {
				t.Fatalf("bit %d distance %d limit %d set=%v", i, d, limit, set)
			}
		}
	}
}

func TestPDQBlockMaskCountClamp(t *testing.T) {
	base := make([]byte, BlockRecords*32)
	var q [32]byte
	// A count above the block size is clamped, never over-read.
	mask := Current.PDQBlockMask(&q, base, BlockRecords+128, 0)
	if mask != ^uint64(0) {
		t.Fatalf("all-zero query and base with limit 0: mask = %#x, want all set", mask)
	}
}
