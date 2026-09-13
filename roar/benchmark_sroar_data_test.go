package roar

import (
	"math/rand"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/weaviate/sroar"
)

// Head-to-head benchmarks on sroar's own testing data.
//
// Each fixture below mirrors a scenario from the sroar repository
// (github.com/weaviate/sroar v0.0.16) with the same distributions and seeds,
// so both libraries chew on identical ID sets:
//
//   - intersection pair: BenchmarkIntersectionRoaring in sroar's
//     benchmark_test.go — 65_000 uniform samples over 150_000 (math/rand
//     source 0) intersected with 65_000 uniform samples over 100_000_000.
//   - merge sets: BenchmarkMerge10K in sroar's benchmark_test.go —
//     1_000 values uniform over 1e8 per bitmap (reduced bitmap count so a
//     single op stays in the millisecond range).
//   - dense range: BenchmarkRemoveRange / BenchmarkSelectSroar in sroar's
//     benchmark_test.go — the contiguous range [0, 100_000).
//
// sroar's And/Or/AndNot mutate the receiver, so each iteration clones the
// operands with the timer stopped; the timed section is the kernel work only,
// matching roar's pure functions. RemoveRange is the exception: sroar only
// offers the mutating form, so the clone is part of the honest end-to-end
// cost there and stays inside the timer.

var (
	sroarDataOnce sync.Once
	sroarDataS1   []uint64
	sroarDataS2   []uint64
	sroarDataRoar [2]Bitmap
	sroarDataRef  [2]*sroar.Bitmap
)

func sortedUniqueNoZero(ids []uint64) []uint64 {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) != 0 && ids[0] == InvalidImageID {
		ids = ids[1:]
	}
	return ids
}

func initSroarData() {
	sroarDataOnce.Do(func() {
		rng := rand.New(rand.NewSource(0))
		s1 := make([]uint64, 0, 65000)
		for range 65000 {
			s1 = append(s1, uint64(rng.Int63n(150000)))
		}
		s2 := make([]uint64, 0, 65000)
		for range 65000 {
			s2 = append(s2, uint64(rng.Int63n(100000000)))
		}
		sroarDataS1 = sortedUniqueNoZero(s1)
		sroarDataS2 = sortedUniqueNoZero(s2)
		var err error
		if sroarDataRoar[0], err = FromSorted(sroarDataS1); err != nil {
			panic(err)
		}
		if sroarDataRoar[1], err = FromSorted(sroarDataS2); err != nil {
			panic(err)
		}
		sroarDataRef[0] = sroar.FromSortedList(sroarDataS1)
		sroarDataRef[1] = sroar.FromSortedList(sroarDataS2)
		if got, want := sroarDataRoar[0].Cardinality(), uint64(sroarDataRef[0].GetCardinality()); got != want {
			panic("fixture cardinality mismatch")
		}
	})
}

func sroarMergeSets(tb testing.TB, count, per int, domain uint64, seed int64) ([]Bitmap, []*sroar.Bitmap) {
	tb.Helper()
	rng := rand.New(rand.NewSource(seed))
	roarSets := make([]Bitmap, 0, count)
	sroarSets := make([]*sroar.Bitmap, 0, count)
	for range count {
		ids := make([]uint64, 0, per)
		for range per {
			ids = append(ids, rng.Uint64()%domain)
		}
		ids = sortedUniqueNoZero(ids)
		bitmap, err := FromSorted(ids)
		if err != nil {
			tb.Fatal(err)
		}
		roarSets = append(roarSets, bitmap)
		sroarSets = append(sroarSets, sroar.FromSortedList(ids))
	}
	return roarSets, sroarSets
}

func sroarDense(tb testing.TB, n uint64) (Bitmap, *sroar.Bitmap) {
	tb.Helper()
	ids := make([]uint64, 0, n)
	for id := range n {
		ids = append(ids, id+1) // +1: roar reserves ImageID 0
	}
	bitmap, err := FromSorted(ids)
	if err != nil {
		tb.Fatal(err)
	}
	return bitmap, sroar.FromSortedList(ids)
}

// cloneSroarPair clones both operands with the timer stopped so only kernel
// work is measured.
func cloneSroarPair(b *testing.B, a, c *sroar.Bitmap) (*sroar.Bitmap, *sroar.Bitmap) {
	b.StopTimer()
	ca, cc := a.Clone(), c.Clone()
	b.StartTimer()
	return ca, cc
}

func BenchmarkSroarDataBuild(b *testing.B) {
	initSroarData()
	b.Run("roar/FromSorted", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = FromSorted(sroarDataS1)
		}
	})
	b.Run("sroar/FromSortedList", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sroar.FromSortedList(sroarDataS1)
		}
	})
}

func BenchmarkSroarDataBuildDense(b *testing.B) {
	const n = 100_000
	ids := make([]uint64, 0, n)
	for id := range uint64(n) {
		ids = append(ids, id+1)
	}
	b.Run("roar/FromSorted", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = FromSorted(ids)
		}
	})
	b.Run("sroar/FromSortedList", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sroar.FromSortedList(ids)
		}
	})
}

func BenchmarkSroarDataAnd(b *testing.B) {
	initSroarData()
	a, c := sroarDataRoar[0], sroarDataRoar[1]
	sa, sc := sroarDataRef[0], sroarDataRef[1]
	if got, want := And(a, c).Cardinality(), uint64(sa.Clone().And(sc.Clone()).GetCardinality()); got != want {
		b.Fatalf("And cardinality mismatch: roar=%d sroar=%d", got, want)
	}
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = And(a, c)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ca, cc := cloneSroarPair(b, sa, sc)
			_ = ca.And(cc)
		}
	})
}

func BenchmarkSroarDataOr(b *testing.B) {
	initSroarData()
	a, c := sroarDataRoar[0], sroarDataRoar[1]
	sa, sc := sroarDataRef[0], sroarDataRef[1]
	if got, want := Or(a, c).Cardinality(), uint64(sa.Clone().Or(sc.Clone()).GetCardinality()); got != want {
		b.Fatalf("Or cardinality mismatch: roar=%d sroar=%d", got, want)
	}
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = Or(a, c)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ca, cc := cloneSroarPair(b, sa, sc)
			_ = ca.Or(cc)
		}
	})
}

func BenchmarkSroarDataAndNot(b *testing.B) {
	initSroarData()
	a, c := sroarDataRoar[0], sroarDataRoar[1]
	sa, sc := sroarDataRef[0], sroarDataRef[1]
	if got, want := AndNot(a, c).Cardinality(), uint64(sa.Clone().AndNot(sc.Clone()).GetCardinality()); got != want {
		b.Fatalf("AndNot cardinality mismatch: roar=%d sroar=%d", got, want)
	}
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndNot(a, c)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ca, cc := cloneSroarPair(b, sa, sc)
			_ = ca.AndNot(cc)
		}
	})
}

func BenchmarkSroarDataIntersects(b *testing.B) {
	initSroarData()
	a, c := sroarDataRoar[0], sroarDataRoar[1]
	sa, sc := sroarDataRef[0], sroarDataRef[1]
	if got, want := Intersects(a, c), sa.Intersects(sc); got != want {
		b.Fatalf("Intersects mismatch: roar=%v sroar=%v", got, want)
	}
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = Intersects(a, c)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sa.Intersects(sc)
		}
	})
}

func BenchmarkSroarDataCardinality(b *testing.B) {
	initSroarData()
	a, c := sroarDataRoar[0], sroarDataRoar[1]
	sa, sc := sroarDataRef[0], sroarDataRef[1]
	b.Run("And/roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndCardinality(a, c)
		}
	})
	// sroar has no AndCardinality; materializing And is the closest
	// equivalent for counting an intersection.
	b.Run("And/sroar-materialized", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			ca, cc := cloneSroarPair(b, sa, sc)
			_ = ca.And(cc).GetCardinality()
		}
	})
	b.Run("Cardinality/roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = a.Cardinality()
		}
	})
	b.Run("GetCardinality/sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sa.GetCardinality()
		}
	})
}

func BenchmarkSroarDataFastOr(b *testing.B) {
	roarSets, sroarSets := sroarMergeSets(b, 500, 1000, 1e8, 1)
	if got, want := OrMany(roarSets...).Cardinality(), uint64(sroar.FastOr(sroarSets...).GetCardinality()); got != want {
		b.Fatalf("FastOr cardinality mismatch: roar=%d sroar=%d", got, want)
	}
	b.Run("roar/OrMany", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = OrMany(roarSets...)
		}
	})
	b.Run("sroar/FastOr", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sroar.FastOr(sroarSets...)
		}
	})
	b.Run("roar/FastParOr4", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = FastParOr(4, roarSets...)
		}
	})
	b.Run("sroar/FastParOr4", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sroar.FastParOr(4, sroarSets...)
		}
	})
}

func BenchmarkSroarDataFastAnd(b *testing.B) {
	// Overlapping sets so the intersection is non-trivial: 4 bitmaps of
	// 65_000 uniform samples over 150_000, same seed family as sroar's
	// intersection data.
	rng := rand.New(rand.NewSource(0))
	var roarSets []Bitmap
	var sroarSets []*sroar.Bitmap
	for range 4 {
		ids := make([]uint64, 0, 65000)
		for range 65000 {
			ids = append(ids, uint64(rng.Int63n(150000)))
		}
		ids = sortedUniqueNoZero(ids)
		bitmap, err := FromSorted(ids)
		if err != nil {
			b.Fatal(err)
		}
		roarSets = append(roarSets, bitmap)
		sroarSets = append(sroarSets, sroar.FromSortedList(ids))
	}
	clones := make([]*sroar.Bitmap, len(sroarSets))
	for i := range sroarSets {
		clones[i] = sroarSets[i].Clone()
	}
	if got, want := AndMany(roarSets...).Cardinality(), uint64(sroar.FastAnd(clones...).GetCardinality()); got != want {
		b.Fatalf("FastAnd cardinality mismatch: roar=%d sroar=%d", got, want)
	}
	b.Run("roar/AndMany", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndMany(roarSets...)
		}
	})
	b.Run("sroar/FastAnd", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			b.StopTimer()
			for i := range sroarSets {
				clones[i] = sroarSets[i].Clone()
			}
			b.StartTimer()
			_ = sroar.FastAnd(clones...)
		}
	})
}

func BenchmarkSroarDataRemoveRange(b *testing.B) {
	bitmap, ref := sroarDense(b, 100_000)
	for _, factor := range []uint64{2, 4, 16, 256} {
		sz := uint64(100_000 / factor)
		b.Run("roar/N="+strconv.FormatUint(factor, 10), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				result := bitmap
				for i := uint64(0); i < 100_000/sz; i++ {
					result = result.RemoveRange(i*sz+1, (i+1)*sz+1)
				}
				_ = result
			}
		})
		b.Run("sroar/N="+strconv.FormatUint(factor, 10), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				clone := ref.Clone()
				for i := uint64(0); i < 100_000/sz; i++ {
					clone.RemoveRange(i*sz+1, (i+1)*sz+1)
				}
				_ = clone
			}
		})
	}
}

func BenchmarkSroarDataSelect(b *testing.B) {
	const n = 100_000
	bitmap, ref := sroarDense(b, n)
	const probes = 1024
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for i := range uint64(probes) {
				_, _ = bitmap.Select(i * (n / probes))
			}
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for i := range uint64(probes) {
				_, _ = ref.Select(i * (n / probes))
			}
		}
	})
}

func BenchmarkSroarDataContains(b *testing.B) {
	initSroarData()
	a := sroarDataRoar[0]
	sa := sroarDataRef[0]
	rng := rand.New(rand.NewSource(7))
	probes := make([]uint64, 4096)
	ids := a.AppendTo(nil)
	for i := range probes {
		if i%2 == 0 {
			probes[i] = ids[rng.Intn(len(ids))]
		} else {
			probes[i] = uint64(rng.Int63n(100000000)) + 1
		}
	}
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = a.Contains(probes[n%len(probes)])
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = sa.Contains(probes[n%len(probes)])
		}
	})
}

func BenchmarkSroarDataIterate(b *testing.B) {
	initSroarData()
	a := sroarDataRoar[0]
	sa := sroarDataRef[0]
	b.Run("roar/AppendTo-reuse", func(b *testing.B) {
		b.ReportAllocs()
		dst := make([]uint64, 0, a.Cardinality())
		for range b.N {
			dst = a.AppendTo(dst[:0])
		}
	})
	b.Run("roar/AppendTo-alloc", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = a.AppendTo(nil)
		}
	})
	b.Run("sroar/ToArray", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sa.ToArray()
		}
	})
}

func BenchmarkSroarDataRank(b *testing.B) {
	// Throughput only: semantics differ (roar counts IDs <= x; sroar returns
	// -1 when x itself is absent), so this is not a fair race.
	initSroarData()
	a := sroarDataRoar[0]
	sa := sroarDataRef[0]
	ids := a.AppendTo(nil)
	rng := rand.New(rand.NewSource(9))
	probes := make([]uint64, 1024)
	for i := range probes {
		probes[i] = ids[rng.Intn(len(ids))]
	}
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = a.Rank(probes[n%len(probes)])
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = sa.Rank(probes[n%len(probes)])
		}
	})
}

func BenchmarkSroarDataMinMax(b *testing.B) {
	initSroarData()
	a := sroarDataRoar[0]
	sa := sroarDataRef[0]
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = a.Minimum()
			_, _ = a.Maximum()
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sa.Minimum()
			_ = sa.Maximum()
		}
	})
}

// BenchmarkSroarDataEndToEnd compares full API cost: roar's pure op against
// sroar's clone-plus-mutate sequence with everything inside the timer. Unlike
// the kernel-only benchmarks above, this is what a caller actually pays.
func BenchmarkSroarDataEndToEnd(b *testing.B) {
	initSroarData()
	a, c := sroarDataRoar[0], sroarDataRoar[1]
	sa, sc := sroarDataRef[0], sroarDataRef[1]
	for _, op := range []struct {
		name  string
		roar  func() Bitmap
		sroar func() *sroar.Bitmap
	}{
		{"And", func() Bitmap { return And(a, c) }, func() *sroar.Bitmap { return sa.Clone().And(sc.Clone()) }},
		{"Or", func() Bitmap { return Or(a, c) }, func() *sroar.Bitmap { return sa.Clone().Or(sc.Clone()) }},
		{"AndNot", func() Bitmap { return AndNot(a, c) }, func() *sroar.Bitmap { return sa.Clone().AndNot(sc.Clone()) }},
	} {
		b.Run(op.name+"/roar", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = op.roar()
			}
		})
		b.Run(op.name+"/sroar", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = op.sroar()
			}
		})
	}
}

// BenchmarkSroarDataRemoveRangesBatch compares removing N ranges from the
// dense 100K fixture: roar's single-pass RemoveRanges against sequential
// RemoveRange loops on both libraries.
func BenchmarkSroarDataRemoveRangesBatch(b *testing.B) {
	bitmap, ref := sroarDense(b, 100_000)
	for _, factor := range []uint64{4, 16, 256} {
		sz := uint64(100_000 / factor)
		var ranges []IDRange
		for i := uint64(0); i < 100_000/sz; i++ {
			ranges = append(ranges, IDRange{Lo: i*sz + 1, Hi: (i+1)*sz + 1})
		}
		name := strconv.FormatUint(factor, 10)
		b.Run("roar/batch-N="+name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = bitmap.RemoveRanges(ranges)
			}
		})
		b.Run("roar/loop-N="+name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				result := bitmap
				for _, r := range ranges {
					result = result.RemoveRange(r.Lo, r.Hi)
				}
				_ = result
			}
		})
		b.Run("sroar/loop-N="+name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				clone := ref.Clone()
				for _, r := range ranges {
					clone.RemoveRange(r.Lo, r.Hi)
				}
				_ = clone
			}
		})
	}
}
