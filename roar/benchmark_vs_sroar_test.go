package roar

import (
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/weaviate/sroar"
)

// Head-to-head regression benchmarks against weaviate/sroar. These guard the
// performance characteristics of the core operations so a refactor that
// silently regresses a hot path shows up in CI.

var (
	vsSroarOnce  sync.Once
	vsSroarRoar  []Bitmap
	vsSroarSroar []*sroar.Bitmap
	vsSroarIDs   [][]uint64
)

func initVsSroar() {
	rng := rand.New(rand.NewPCG(2024, 7))
	pool := keyPool(rng, 24)
	const count = 4
	vsSroarIDs = make([][]uint64, count)
	vsSroarRoar = make([]Bitmap, count)
	vsSroarSroar = make([]*sroar.Bitmap, count)
	for index := range count {
		ids := randomSetOver(rng, pool)
		vsSroarIDs[index] = ids
		vsSroarRoar[index], _ = FromSorted(ids)
		vsSroarSroar[index] = sroar.FromSortedList(ids)
	}
}

func vsSroarData() ([]Bitmap, []*sroar.Bitmap) {
	vsSroarOnce.Do(initVsSroar)
	return vsSroarRoar, vsSroarSroar
}

func BenchmarkVsSroarBuild(b *testing.B) {
	_, _ = vsSroarData()
	ids := vsSroarIDs[0]
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_, _ = FromSorted(ids)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sroar.FromSortedList(ids)
		}
	})
}

func BenchmarkVsSroarContains(b *testing.B) {
	roarBm, sroarBm := vsSroarData()
	bitmap := roarBm[0]
	ref := sroarBm[0]
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = bitmap.Contains(uint64(n%1_000_000 + 1))
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			_ = ref.Contains(uint64(n%1_000_000 + 1))
		}
	})
}

func BenchmarkVsSroarIterate(b *testing.B) {
	roarBm, sroarBm := vsSroarData()
	bitmap := roarBm[0]
	ref := sroarBm[0]
	b.Run("roar", func(b *testing.B) {
		dst := make([]uint64, 0, bitmap.Cardinality())
		b.ReportAllocs()
		for range b.N {
			dst = bitmap.AppendTo(dst[:0])
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = ref.ToArray()
		}
	})
}

func benchmarkVsSroarBinary(b *testing.B, op string, roarOp func(a, b Bitmap) Bitmap, sroarOp func(a, b *sroar.Bitmap) *sroar.Bitmap) {
	roarBm, sroarBm := vsSroarData()
	a, c := roarBm[0], roarBm[1]
	sa, sc := sroarBm[0], sroarBm[1]
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = roarOp(a, c)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = sroarOp(sa.Clone(), sc.Clone())
		}
	})
}

func BenchmarkVsSroarAnd(b *testing.B) {
	benchmarkVsSroarBinary(b, "And", And, func(a, b *sroar.Bitmap) *sroar.Bitmap { return a.And(b) })
}

func BenchmarkVsSroarOr(b *testing.B) {
	benchmarkVsSroarBinary(b, "Or", Or, func(a, b *sroar.Bitmap) *sroar.Bitmap { return a.Or(b) })
}

func BenchmarkVsSroarAndNot(b *testing.B) {
	benchmarkVsSroarBinary(b, "AndNot", AndNot, func(a, b *sroar.Bitmap) *sroar.Bitmap { return a.AndNot(b) })
}

func BenchmarkVsSroarIntersects(b *testing.B) {
	roarBm, sroarBm := vsSroarData()
	a, c := roarBm[0], roarBm[1]
	sa, sc := sroarBm[0], sroarBm[1]
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

func BenchmarkVsSroarAndMany(b *testing.B) {
	roarBm, sroarBm := vsSroarData()
	b.Run("roar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndMany(roarBm...)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			clones := make([]*sroar.Bitmap, len(sroarBm))
			for i := range sroarBm {
				clones[i] = sroarBm[i].Clone()
			}
			_ = sroar.FastAnd(clones...)
		}
	})
}

// BenchmarkDeltaVsBitmap compares the same whole-set op run through the
// immutable free function versus the single-owner DeltaBuilder method.
func BenchmarkDeltaVsBitmap(b *testing.B) {
	roarBm, _ := vsSroarData()
	a, c := roarBm[0], roarBm[1]

	ops := []struct {
		name   string
		bitmap func(a, b Bitmap) Bitmap
		delta  func(m *DeltaBuilder, b Bitmap)
	}{
		{"And", And, func(m *DeltaBuilder, b Bitmap) { m.And(b) }},
		{"Or", Or, func(m *DeltaBuilder, b Bitmap) { m.Or(b) }},
		{"AndNot", AndNot, func(m *DeltaBuilder, b Bitmap) { m.AndNot(b) }},
	}
	for _, op := range ops {
		b.Run(op.name+"/bitmap", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = op.bitmap(a, c)
			}
		})
		b.Run(op.name+"/delta", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				m := NewDeltaBuilderFrom(a)
				op.delta(m, c)
				_ = m.Freeze()
			}
		})
	}
}

// BenchmarkVsSroarAndChain compares building an N-way intersection as a
// sequential fold: DeltaBuilder (reused builder), chained And (fresh builder
// per step), and AndMany (single pass).
func BenchmarkVsSroarAndChain(b *testing.B) {
	roarBm, sroarBm := vsSroarData()
	b.Run("delta", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			m := NewDeltaBuilderFrom(roarBm[0])
			for _, bm := range roarBm[1:] {
				m.And(bm)
			}
			_ = m.Freeze()
		}
	})
	b.Run("chained", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			result := roarBm[0]
			for _, bm := range roarBm[1:] {
				result = And(result, bm)
			}
		}
	})
	b.Run("andMany", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndMany(roarBm...)
		}
	})
	b.Run("sroar", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			clones := make([]*sroar.Bitmap, len(sroarBm))
			for i := range sroarBm {
				clones[i] = sroarBm[i].Clone()
			}
			_ = sroar.FastAnd(clones...)
		}
	})
}
