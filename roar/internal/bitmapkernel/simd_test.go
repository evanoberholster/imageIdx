//go:build goexperiment.simd

package bitmapkernel

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestSIMDMatchesScalar compares the active SIMD backend (selected at init)
// against the scalar reference across every shape. On a machine where SIMD is
// not active the two are the same and the test is trivially satisfied.
func TestSIMDMatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))
	shapes := []string{"zero", "ones", "single", "sparse", "dense", "alternating"}
	for _, sa := range shapes {
		for _, sb := range shapes {
			a := makeShape(sa, rng)
			b := makeShape(sb, rng)
			c := makeShape("dense", rng)
			got := make([]uint64, testWords)
			want := make([]uint64, testWords)

			if andKernel(got, a, b) != andScalar(want, a, b) || !slices.Equal(got, want) {
				t.Fatalf("and(%s,%s) mismatch", sa, sb)
			}
			if orKernel(got, a, b) != orScalar(want, a, b) || !slices.Equal(got, want) {
				t.Fatalf("or(%s,%s) mismatch", sa, sb)
			}
			if andNotKernel(got, a, b) != andNotScalar(want, a, b) || !slices.Equal(got, want) {
				t.Fatalf("andnot(%s,%s) mismatch", sa, sb)
			}
			if intersectsKernel(a, b) != intersectsScalar(a, b) {
				t.Fatalf("intersects(%s,%s) mismatch", sa, sb)
			}
			if andCardinalityKernel(a, b) != andCardinalityScalar(a, b) {
				t.Fatalf("andCardinality(%s,%s) mismatch", sa, sb)
			}
			if orCardinalityKernel(a, b) != orCardinalityScalar(a, b) {
				t.Fatalf("orCardinality(%s,%s) mismatch", sa, sb)
			}
			if andNotCardinalityKernel(a, b) != andNotCardinalityScalar(a, b) {
				t.Fatalf("andNotCardinality(%s,%s) mismatch", sa, sb)
			}
			if andManyKernel(got, a, b, c) != andManyScalar(want, a, b, c) || !slices.Equal(got, want) {
				t.Fatalf("andMany(%s,%s) mismatch", sa, sb)
			}
			if applyKernel(got, a, b, c) != applyScalar(want, a, b, c) || !slices.Equal(got, want) {
				t.Fatalf("apply(%s,%s) mismatch", sa, sb)
			}
		}
	}
}

func TestSIMDTails(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 19))
	for n := 0; n <= 129; n++ {
		a := make([]uint64, n)
		b := make([]uint64, n)
		c := make([]uint64, n)
		for i := range a {
			a[i], b[i], c[i] = rng.Uint64(), rng.Uint64(), rng.Uint64()
		}
		got := make([]uint64, n)
		want := make([]uint64, n)

		if andKernel(got, a, b) != andScalar(want, a, b) || !slices.Equal(got, want) {
			t.Fatalf("and length %d mismatch", n)
		}
		if orKernel(got, a, b) != orScalar(want, a, b) || !slices.Equal(got, want) {
			t.Fatalf("or length %d mismatch", n)
		}
		if andNotKernel(got, a, b) != andNotScalar(want, a, b) || !slices.Equal(got, want) {
			t.Fatalf("andnot length %d mismatch", n)
		}
		if intersectsKernel(a, b) != intersectsScalar(a, b) {
			t.Fatalf("intersects length %d mismatch", n)
		}
		if andCardinalityKernel(a, b) != andCardinalityScalar(a, b) ||
			orCardinalityKernel(a, b) != orCardinalityScalar(a, b) ||
			andNotCardinalityKernel(a, b) != andNotCardinalityScalar(a, b) ||
			cardinalityKernel(a) != cardinalityScalar(a) {
			t.Fatalf("cardinality length %d mismatch", n)
		}
		if applyKernel(got, a, b, c) != applyScalar(want, a, b, c) || !slices.Equal(got, want) {
			t.Fatalf("apply length %d mismatch", n)
		}
		if andMany3Kernel(got, a, b, c) != andMany3Scalar(want, a, b, c) || !slices.Equal(got, want) {
			t.Fatalf("andMany3 length %d mismatch", n)
		}
		if andMany4Kernel(got, a, b, c, a) != andMany4Scalar(want, a, b, c, a) || !slices.Equal(got, want) {
			t.Fatalf("andMany4 length %d mismatch", n)
		}
	}
}
