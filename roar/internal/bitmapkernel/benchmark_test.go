package bitmapkernel

import (
	"math/rand/v2"
	"strconv"
	"testing"
)

func benchContainers(rng *rand.Rand, dense bool) (a, b, c, dst []uint64) {
	a = make([]uint64, testWords)
	b = make([]uint64, testWords)
	c = make([]uint64, testWords)
	dst = make([]uint64, testWords)
	for i := range a {
		if dense {
			a[i] = rng.Uint64()
			b[i] = rng.Uint64()
			c[i] = rng.Uint64()
		} else {
			if rng.IntN(64) == 0 {
				a[i] = uint64(1) << rng.IntN(64)
			}
			if rng.IntN(64) == 0 {
				b[i] = uint64(1) << rng.IntN(64)
			}
			if rng.IntN(64) == 0 {
				c[i] = uint64(1) << rng.IntN(64)
			}
		}
	}
	return a, b, c, dst
}

func BenchmarkAnd(b *testing.B) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, _, dst := benchContainers(rng, dense)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = And(dst, a, bb)
			}
		})
	}
}

func BenchmarkOr(b *testing.B) {
	rng := rand.New(rand.NewPCG(2, 3))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, _, dst := benchContainers(rng, dense)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = Or(dst, a, bb)
			}
		})
	}
}

func BenchmarkAndNot(b *testing.B) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, _, dst := benchContainers(rng, dense)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = AndNot(dst, a, bb)
			}
		})
	}
}

func BenchmarkIntersects(b *testing.B) {
	rng := rand.New(rand.NewPCG(4, 5))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, _, _ := benchContainers(rng, dense)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = Intersects(a, bb)
			}
		})
	}
}

func BenchmarkAndMany(b *testing.B) {
	rng := rand.New(rand.NewPCG(5, 6))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, c, dst := benchContainers(rng, dense)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = AndMany3(dst, a, bb, c)
			}
		})
	}
}

func BenchmarkAndMany4(b *testing.B) {
	rng := rand.New(rand.NewPCG(15, 16))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, c, dst := benchContainers(rng, dense)
		d := make([]uint64, testWords)
		for i := range d {
			if dense {
				d[i] = rng.Uint64()
			} else if rng.IntN(64) == 0 {
				d[i] = uint64(1) << rng.IntN(64)
			}
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = AndMany4(dst, a, bb, c, d)
			}
		})
	}
}

func BenchmarkAndManyCardinality(b *testing.B) {
	rng := rand.New(rand.NewPCG(16, 17))
	a, bb, c, _ := benchContainers(rng, true)
	d := make([]uint64, testWords)
	for i := range d {
		d[i] = rng.Uint64()
	}
	b.Run("3", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndMany3Cardinality(a, bb, c)
		}
	})
	b.Run("4", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = AndMany4Cardinality(a, bb, c, d)
		}
	})
}

func BenchmarkCardinalityPrefix(b *testing.B) {
	rng := rand.New(rand.NewPCG(18, 19))
	a, _, _, _ := benchContainers(rng, true)
	for _, wordCount := range []int{512, testWords} {
		b.Run(strconv.Itoa(wordCount), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = CardinalityPrefix(a, wordCount)
			}
		})
	}
}

func BenchmarkAndManyVariadic4(b *testing.B) {
	rng := rand.New(rand.NewPCG(17, 18))
	a, bb, c, dst := benchContainers(rng, true)
	d := make([]uint64, testWords)
	for i := range d {
		d[i] = rng.Uint64()
	}
	b.ReportAllocs()
	for range b.N {
		_ = AndMany(dst, a, bb, c, d)
	}
}

func BenchmarkAndManyVariadic3(b *testing.B) {
	rng := rand.New(rand.NewPCG(19, 20))
	a, bb, c, dst := benchContainers(rng, true)
	b.ReportAllocs()
	for range b.N {
		_ = AndMany(dst, a, bb, c)
	}
}

func BenchmarkAndManyVariadic5To8(b *testing.B) {
	rng := rand.New(rand.NewPCG(21, 22))
	inputs := make([][]uint64, 8)
	for index := range inputs {
		inputs[index] = make([]uint64, testWords)
		for word := range inputs[index] {
			inputs[index][word] = rng.Uint64()
		}
	}
	dst := make([]uint64, testWords)
	for count := 5; count <= len(inputs); count++ {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = AndMany(dst, inputs[:count]...)
			}
		})
	}
}

func BenchmarkApply(b *testing.B) {
	rng := rand.New(rand.NewPCG(6, 7))
	for _, dense := range []bool{false, true} {
		name := "sparse"
		if dense {
			name = "dense"
		}
		a, bb, c, dst := benchContainers(rng, dense)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = Apply(dst, a, bb, c)
			}
		})
	}
}

// BenchmarkScalarBaseline measures the scalar backend directly, for comparing
// against the active backend in a SIMD build.
func BenchmarkScalarBaseline(b *testing.B) {
	rng := rand.New(rand.NewPCG(9, 9))
	a, bb, _, dst := benchContainers(rng, true)
	b.ReportAllocs()
	for range b.N {
		_ = andScalar(dst, a, bb)
	}
}
