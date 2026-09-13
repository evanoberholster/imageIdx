package bitmapkernel

import (
	"math/bits"
	"math/rand/v2"
	"slices"
	"testing"
)

// Reference implementations over math/bits, independent of the package's
// scalar and SIMD backends.

func refAnd(dst, a, b []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := a[i] & b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func refOr(dst, a, b []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := a[i] | b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func refAndNot(dst, a, b []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := a[i] &^ b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func refIntersects(a, b []uint64) bool {
	for i := range a {
		if a[i]&b[i] != 0 {
			return true
		}
	}
	return false
}

const testWords = 1024

// makeShape returns a test container matching the named shape.
func makeShape(shape string, rng *rand.Rand) []uint64 {
	w := make([]uint64, testWords)
	switch shape {
	case "zero":
		// all zero
	case "ones":
		for i := range w {
			w[i] = ^uint64(0)
		}
	case "single":
		w[rng.IntN(testWords)] = uint64(1) << rng.IntN(64)
	case "sparse":
		for range 64 {
			w[rng.IntN(testWords)] |= uint64(1) << rng.IntN(64)
		}
	case "dense":
		for i := range w {
			w[i] = rng.Uint64()
		}
	case "alternating":
		for i := range w {
			if i%2 == 0 {
				w[i] = 0xAAAAAAAAAAAAAAAA
			} else {
				w[i] = 0x5555555555555555
			}
		}
	default:
		panic("unknown shape " + shape)
	}
	return w
}

func TestKernelsAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	pairs := [][2]string{
		{"zero", "zero"}, {"zero", "ones"}, {"ones", "ones"},
		{"sparse", "sparse"}, {"dense", "dense"}, {"sparse", "dense"},
		{"alternating", "alternating"}, {"alternating", "dense"},
	}
	for _, p := range pairs {
		t.Run(p[0]+"_"+p[1], func(t *testing.T) {
			a := makeShape(p[0], rng)
			b := makeShape(p[1], rng)

			// Identical and disjoint operands.
			aClone := slices.Clone(a)
			bZero := make([]uint64, testWords)

			gotDst := make([]uint64, testWords)
			wantDst := make([]uint64, testWords)

			if got := And(gotDst, a, b); got != refAnd(wantDst, a, b) || !slices.Equal(gotDst, wantDst) {
				t.Fatal("And mismatch")
			}
			if got := Or(gotDst, a, b); got != refOr(wantDst, a, b) || !slices.Equal(gotDst, wantDst) {
				t.Fatal("Or mismatch")
			}
			if got := AndNot(gotDst, a, b); got != refAndNot(wantDst, a, b) || !slices.Equal(gotDst, wantDst) {
				t.Fatal("AndNot mismatch")
			}
			if Intersects(a, b) != refIntersects(a, b) {
				t.Fatal("Intersects mismatch")
			}
			if Intersects(a, bZero) != refIntersects(a, bZero) {
				t.Fatal("Intersects(disjoint) mismatch")
			}
			if AndCardinality(a, b) != uint64(refAnd(wantDst, a, b)) {
				t.Fatal("AndCardinality mismatch")
			}
			if OrCardinality(a, b) != uint64(refOr(wantDst, a, b)) {
				t.Fatal("OrCardinality mismatch")
			}
			if AndNotCardinality(a, b) != uint64(refAndNot(wantDst, a, b)) {
				t.Fatal("AndNotCardinality mismatch")
			}
			// Identical operands: And == identity, cardinality matches.
			if got := And(gotDst, a, aClone); uint64(got) != Cardinality(a) || !slices.Equal(gotDst, a) {
				t.Fatal("And(identical) mismatch")
			}
			// Cardinality.
			if Cardinality(a) != uint64(refOr(wantDst, a, bZero)) {
				t.Fatal("Cardinality mismatch")
			}
			for _, wordCount := range []int{0, 1, 7, 8, 63, 64, 511, 512, testWords} {
				if CardinalityPrefix(a, wordCount) != cardinalityPrefixScalar(a, wordCount) {
					t.Fatalf("CardinalityPrefix(%d) mismatch", wordCount)
				}
			}
		})
	}
}

func TestFusedKernelsAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for round := 0; round < 20; round++ {
		base := makeShape("dense", rng)
		add := makeShape("sparse", rng)
		remove := makeShape("dense", rng)
		c := makeShape("dense", rng)
		d := makeShape("sparse", rng)

		got := make([]uint64, testWords)
		want := make([]uint64, testWords)

		// Apply: (base | add) &^ remove.
		gotCard := Apply(got, base, add, remove)
		wantCard := refAndNot(want, refOrInto(base, add), remove)
		if gotCard != wantCard || !slices.Equal(got, want) {
			t.Fatalf("Apply round %d mismatch", round)
		}

		// AndMany: base & c & d.
		gotCard = AndMany3(got, base, c, d)
		wantCard = refAnd(want, refAndInto(base, c), d)
		if gotCard != wantCard || !slices.Equal(got, want) {
			t.Fatalf("AndMany3 round %d mismatch", round)
		}
		gotCard = AndMany4(got, base, c, d, remove)
		wantCard = refAnd(want, refAndInto(refAndInto(base, c), d), remove)
		if gotCard != wantCard || !slices.Equal(got, want) {
			t.Fatalf("AndMany4 round %d mismatch", round)
		}
		wantCard = refAnd(want, refAndInto(base, c), d)
		if got := AndMany3Cardinality(base, c, d); got != uint64(wantCard) {
			t.Fatalf("AndMany3Cardinality round %d mismatch", round)
		}
		wantCard = refAnd(want, refAndInto(refAndInto(base, c), d), remove)
		if got := AndMany4Cardinality(base, c, d, remove); got != uint64(wantCard) {
			t.Fatalf("AndMany4Cardinality round %d mismatch", round)
		}
	}
}

func TestGenericAndManyAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	inputs := make([][]uint64, 8)
	for index := range inputs {
		inputs[index] = makeShape("dense", rng)
	}

	got := make([]uint64, testWords)
	want := make([]uint64, testWords)
	for count := 5; count <= len(inputs); count++ {
		gotCard := AndMany(got, inputs[:count]...)
		copy(want, inputs[0])
		var wantCard uint32
		for _, input := range inputs[1:count] {
			wantCard = refAnd(want, want, input)
		}
		if gotCard != wantCard || !slices.Equal(got, want) {
			t.Fatalf("AndMany(%d) mismatch", count)
		}
	}
}

func refOrInto(a, b []uint64) []uint64 {
	out := make([]uint64, len(a))
	refOr(out, a, b)
	return out
}

func refAndInto(a, b []uint64) []uint64 {
	out := make([]uint64, len(a))
	refAnd(out, a, b)
	return out
}
