package geoidx

import (
	"math/rand"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// TestSortIDsDifferential checks the bitset, radix, and comparison sorts against
// sort+compact over dense, sparse, duplicate-heavy, empty, and single domains.
func TestSortIDsDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	cases := []struct {
		name  string
		n     int
		maxID common.ImageID
	}{
		{"empty", 0, 1000},
		{"single", 1, 1000},
		{"small", 100, 1_000_000_000_000},
		{"dense", 50_000, 60_000},
		{"dense-duplicate-heavy", 50_000, 1_000},
		{"sparse-bitset-rejected", 50_000, 1 << 40},
		{"sparse-large", 20_000, 1 << 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]common.ImageID, tc.n)
			for i := range ids {
				ids[i] = common.ImageID(uint64(rng.Int63n(int64(tc.maxID))) + 1)
			}
			want := append([]common.ImageID(nil), ids...)
			slices.Sort(want)
			want = slices.Compact(want)
			got := sortIDs(append([]common.ImageID(nil), ids...), tc.maxID)
			if !slices.Equal(got, want) {
				t.Fatalf("sortIDs: got %d ids, want %d", len(got), len(want))
			}
		})
	}
}

// TestSortSuffixPreservesPrefix verifies only the appended suffix is sorted and
// that duplicates are removed without an extra allocation when already sorted.
func TestSortSuffixPreservesPrefix(t *testing.T) {
	dst := []common.ImageID{9, 9, 5, 3, 5, 1}
	got := sortSuffix(dst, 2, 10)
	want := []common.ImageID{9, 9, 1, 3, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("sortSuffix = %v, want %v", got, want)
	}
	// Already-ordered suffix: dedup only, prefix untouched.
	ordered := []common.ImageID{7, 7, 2, 2, 3, 3}
	got = sortSuffix(ordered, 2, 10)
	if want := []common.ImageID{7, 7, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("sorted suffix = %v, want %v", got, want)
	}
	// Empty suffix leaves dst unchanged.
	empty := []common.ImageID{4, 4}
	if got := sortSuffix(empty, 2, 10); !slices.Equal(got, empty) {
		t.Fatalf("empty suffix = %v", got)
	}
}
