package roar

import (
	"slices"
	"testing"
)

func TestFromUnsorted(t *testing.T) {
	cases := []struct {
		name string
		in   []uint64
		want []uint64
	}{
		{"empty", nil, nil},
		{"single", []uint64{9}, []uint64{9}},
		{"sorted", []uint64{1, 2, 1 << 16}, []uint64{1, 2, 1 << 16}},
		{"reversed", []uint64{9, 4, 1}, []uint64{1, 4, 9}},
		{"duplicates", []uint64{5, 5, 3, 5, 3}, []uint64{3, 5}},
		{"zeroDropped", []uint64{0, 3, 0, 1}, []uint64{1, 3}},
		{"onlyZero", []uint64{0, 0}, nil},
		{"sortedWithZero", []uint64{0, 2, 4}, []uint64{2, 4}},
		{"wideKeys", []uint64{1<<48 + 7, 3, 1<<32 + 1, 3}, []uint64{3, 1<<32 + 1, 1<<48 + 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]uint64(nil), tc.in...)
			got := FromUnsorted(tc.in)
			if !slices.Equal(got.AppendTo(nil), tc.want) {
				t.Fatalf("FromUnsorted(%v) = %v, want %v", tc.in, got.AppendTo(nil), tc.want)
			}
			if got.Cardinality() != uint64(len(tc.want)) {
				t.Fatalf("Cardinality = %d, want %d", got.Cardinality(), len(tc.want))
			}
			if !slices.Equal(tc.in, original) {
				t.Fatal("FromUnsorted mutated its input")
			}
		})
	}
}
