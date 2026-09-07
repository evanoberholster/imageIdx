package bitmapidx

import (
	"testing"

	"github.com/evanoberholster/imageIdx/idset"
	"github.com/evanoberholster/imageIdx/roar"
)

// These benchmarks compare the idset wrapper against raw roar on the exact
// set primitives bitmapidx relies on: build, membership, materialization,
// and the intersection/union/mutation patterns behind Intersect, Union, and
// Apply. Both sides share the roar representation; the delta between the two
// sub-benchmarks is the wrapper overhead.

func spacedIDs(count, step, shift int) []uint64 {
	ids := make([]uint64, count)
	for n := range ids {
		// +1 keeps every ID positive; roar reserves ID 0.
		ids[n] = uint64(n*step + shift + 1)
	}
	return ids
}

func benchmarkBackendBuild(b *testing.B) {
	ids := spacedIDs(100_000, 3, 0)
	b.Run("idset", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = idset.New(ids)
		}
	})
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = roar.FromSorted(ids)
		}
	})
}

func benchmarkBackendContains(b *testing.B) {
	ids := spacedIDs(100_000, 3, 0)
	ref := idset.New(ids)
	bitmap, err := roar.FromSorted(ids)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("idset", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = ref.Contains(uint64(n % 300_000))
		}
	})
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = bitmap.Contains(uint64(n % 300_000))
		}
	})
}

func benchmarkBackendAppendTo(b *testing.B) {
	ids := spacedIDs(100_000, 3, 0)
	ref := idset.New(ids)
	bitmap, err := roar.FromSorted(ids)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("idset", func(b *testing.B) {
		dst := make([]uint64, 0, len(ids))
		b.ReportAllocs()
		for range b.N {
			dst = ref.AppendTo(dst[:0])
		}
	})
	b.Run("roar", func(b *testing.B) {
		dst := make([]uint64, 0, len(ids))
		b.ReportAllocs()
		for range b.N {
			dst = bitmap.AppendTo(dst[:0])
		}
	})
}

func backendFourSets(count int) ([]idset.Ref, []roar.Bitmap) {
	refs := make([]idset.Ref, 4)
	bitmaps := make([]roar.Bitmap, 4)
	for n := 0; n < 4; n++ {
		ids := spacedIDs(count, n+1, n)
		refs[n] = idset.New(ids)
		bitmaps[n], _ = roar.FromSorted(ids)
	}
	return refs, bitmaps
}

func benchmarkBackendIntersect4(b *testing.B) {
	refs, bitmaps := backendFourSets(20_000)
	b.Run("idset", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ops := idset.NewOperands(len(refs))
			for _, ref := range refs {
				ops.Add(ref)
			}
			_ = ops.And()
		}
	})
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = roar.AndMany(bitmaps...)
		}
	})
}

func benchmarkBackendUnion4(b *testing.B) {
	refs, bitmaps := backendFourSets(20_000)
	b.Run("idset", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ops := idset.NewOperands(len(refs))
			for _, ref := range refs {
				ops.Add(ref)
			}
			_ = ops.Or()
		}
	})
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = roar.FastParOr(1, bitmaps...)
		}
	})
}

func benchmarkBackendApply(b *testing.B) {
	base := spacedIDs(10_000, 3, 0)
	benchBackendApply(b, base, spacedIDs(1_000, 5, 1), spacedIDs(500, 6, 2))
}

func benchmarkBackendApplySmall(b *testing.B) {
	base := spacedIDs(10_000, 3, 0)
	benchBackendApply(b, base, spacedIDs(32, 5, 1), spacedIDs(16, 6, 2))
}

func benchBackendApply(b *testing.B, base, adds, removes []uint64) {
	baseRef := idset.New(base)
	baseBitmap, err := roar.FromSorted(base)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("idset", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			builder := idset.Clone(baseRef)
			builder.AddMany(adds)
			for _, id := range removes {
				builder.Remove(id)
			}
			_ = builder.Freeze()
		}
	})
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			builder := roar.NewDeltaBuilderFrom(baseBitmap)
			builder.AddMany(adds)
			for _, id := range removes {
				builder.Remove(id)
			}
			_ = builder.Freeze()
		}
	})
}

func BenchmarkBackendBuild(b *testing.B)      { benchmarkBackendBuild(b) }
func BenchmarkBackendContains(b *testing.B)   { benchmarkBackendContains(b) }
func BenchmarkBackendAppendTo(b *testing.B)   { benchmarkBackendAppendTo(b) }
func BenchmarkBackendIntersect4(b *testing.B) { benchmarkBackendIntersect4(b) }
func BenchmarkBackendUnion4(b *testing.B)     { benchmarkBackendUnion4(b) }
func BenchmarkBackendApply(b *testing.B)      { benchmarkBackendApply(b) }
func BenchmarkBackendApplySmall(b *testing.B) { benchmarkBackendApplySmall(b) }
