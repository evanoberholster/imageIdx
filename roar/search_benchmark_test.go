package roar

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/weaviate/sroar"
)

// Search-shaped benchmarks: fold ~20,000-ID bitmaps with a mixed sequence of
// Or and And operations, comparing the immutable Bitmap free functions, the
// single-owner MutableBitmap, and weaviate/sroar's in-place mutation.

const searchBitmapCount = 6

// denseSearchIDs returns ~20,000 consecutive IDs. Consecutive bitmaps overlap
// by half, so they occupy bitmap containers.
func denseSearchIDs(offset int) []uint64 {
	ids := make([]uint64, 20_000)
	base := uint64(1 + offset*10_000)
	for i := range ids {
		ids[i] = base + uint64(i)
	}
	return ids
}

// sparseSearchIDs returns ~20,000 IDs scattered across 200 keys (~100 per
// key), producing array containers with heavy overlap across bitmaps.
func sparseSearchIDs(rng *rand.Rand) []uint64 {
	const keys, values = 200, 1000
	seen := make(map[uint64]struct{}, 20_000)
	for len(seen) < 20_000 {
		key := uint64(1+rng.IntN(keys)) << 16
		seen[key|uint64(1+rng.IntN(values))] = struct{}{}
	}
	ids := make([]uint64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func searchSets(dense bool) ([]Bitmap, []*sroar.Bitmap) {
	roarBm := make([]Bitmap, searchBitmapCount)
	sroarBm := make([]*sroar.Bitmap, searchBitmapCount)
	rng := rand.New(rand.NewPCG(7, 9))
	for i := 0; i < searchBitmapCount; i++ {
		var ids []uint64
		if dense {
			ids = denseSearchIDs(i)
		} else {
			ids = sparseSearchIDs(rng)
		}
		roarBm[i], _ = FromSorted(ids)
		sroarBm[i] = sroar.FromSortedList(ids)
	}
	return roarBm, sroarBm
}

// searchSequence folds the operands with the representative mixed sequence
// ((a OR b OR c) AND d AND e) OR f.
func searchSequence(bitmaps []Bitmap) Bitmap {
	res := bitmaps[0]
	res = Or(res, bitmaps[1])
	res = Or(res, bitmaps[2])
	res = And(res, bitmaps[3])
	res = And(res, bitmaps[4])
	res = Or(res, bitmaps[5])
	return res
}

func searchSequenceMutable(bitmaps []Bitmap) Bitmap {
	m := NewMutableBitmapFrom(bitmaps[0])
	m.Or(bitmaps[1])
	m.Or(bitmaps[2])
	m.And(bitmaps[3])
	m.And(bitmaps[4])
	m.Or(bitmaps[5])
	return m.Freeze()
}

func searchSequenceSroar(bitmaps []*sroar.Bitmap) *sroar.Bitmap {
	res := bitmaps[0].Clone()
	res.Or(bitmaps[1])
	res.Or(bitmaps[2])
	res.And(bitmaps[3])
	res.And(bitmaps[4])
	res.Or(bitmaps[5])
	return res
}

func BenchmarkSearch(b *testing.B) {
	for _, dense := range []bool{true, false} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		roarBm, sroarBm := searchSets(dense)
		b.Run(name+"/bitmap", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = searchSequence(roarBm)
			}
		})
		b.Run(name+"/mutable", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = searchSequenceMutable(roarBm)
			}
		})
		b.Run(name+"/sroar", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = searchSequenceSroar(sroarBm)
			}
		})
	}
}
