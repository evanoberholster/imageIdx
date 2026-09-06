package roar

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func randValues(rng *rand.Rand, count int) []uint16 {
	seen := map[uint16]bool{}
	for len(seen) < count {
		seen[uint16(rng.IntN(2000))] = true
	}
	out := make([]uint16, 0, count)
	for v := range seen {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

func TestGallopBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	for round := 0; round < 20000; round++ {
		a := randValues(rng, rng.IntN(300))
		b := randValues(rng, rng.IntN(300))

		var dst []uint16
		unionArrays(&dst, a, b)
		wu := append(slices.Clone(a), b...)
		slices.Sort(wu)
		wu = slices.Compact(wu)
		if !slices.Equal(dst, wu) {
			t.Fatalf("union round %d: a=%v b=%v got=%v want=%v", round, a, b, dst, wu)
		}

		dst = dst[:0]
		differenceArrays(&dst, a, b)
		set := map[uint16]bool{}
		for _, x := range b {
			set[x] = true
		}
		wd := dst[:0]
		for _, x := range a {
			if !set[x] {
				wd = append(wd, x)
			}
		}
		if !slices.Equal(dst, wd) {
			t.Fatalf("diff round %d: a=%v b=%v got=%v want=%v", round, a, b, dst, wd)
		}

		dst = dst[:0]
		intersectArrays(&dst, a, b)
		wi := dst[:0]
		for _, x := range a {
			if slices.Contains(b, x) {
				wi = append(wi, x)
			}
		}
		if !slices.Equal(dst, wi) {
			t.Fatalf("intersect round %d: a=%v b=%v got=%v want=%v", round, a, b, dst, wi)
		}
	}
}
