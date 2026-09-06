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

			if andImpl(got, a, b) != andScalar(want, a, b) || !slices.Equal(got, want) {
				t.Fatalf("and(%s,%s) mismatch", sa, sb)
			}
			if orImpl(got, a, b) != orScalar(want, a, b) || !slices.Equal(got, want) {
				t.Fatalf("or(%s,%s) mismatch", sa, sb)
			}
			if andNotImpl(got, a, b) != andNotScalar(want, a, b) || !slices.Equal(got, want) {
				t.Fatalf("andnot(%s,%s) mismatch", sa, sb)
			}
			if intersectsImpl(a, b) != intersectsScalar(a, b) {
				t.Fatalf("intersects(%s,%s) mismatch", sa, sb)
			}
			if andCardinalityImpl(a, b) != andCardinalityScalar(a, b) {
				t.Fatalf("andCardinality(%s,%s) mismatch", sa, sb)
			}
			if orCardinalityImpl(a, b) != orCardinalityScalar(a, b) {
				t.Fatalf("orCardinality(%s,%s) mismatch", sa, sb)
			}
			if andNotCardinalityImpl(a, b) != andNotCardinalityScalar(a, b) {
				t.Fatalf("andNotCardinality(%s,%s) mismatch", sa, sb)
			}
			if andManyImpl(got, a, b, c) != andManyScalar(want, a, b, c) || !slices.Equal(got, want) {
				t.Fatalf("andMany(%s,%s) mismatch", sa, sb)
			}
			if applyImpl(got, a, b, c) != applyScalar(want, a, b, c) || !slices.Equal(got, want) {
				t.Fatalf("apply(%s,%s) mismatch", sa, sb)
			}
		}
	}
}
