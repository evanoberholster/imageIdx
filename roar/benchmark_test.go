package roar

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/weaviate/sroar"
)

// Benchmarks compare roar against weaviate/sroar over deterministic,
// photo-database-shaped uint64 distributions:
//
//   - geoCells: many tiny lists of 1-4 IDs per 48-bit key (S2-style cells);
//   - people: small lists of 5-300 IDs;
//   - tags: medium lists of 1k-5k random IDs;
//   - timeBuckets: dense sequential IDs (bitmap containers);
//   - favorites: large lists of 5k-15k random IDs;
//   - mixed: per-key mixture of tiny, sparse, and dense containers.
//
// Every family contains four overlapping lists so pair and N-way
// benchmarks share keys. sroar's And/Or/AndNot mutate their receiver, so
// sroar materializing benchmarks include a Clone to preserve inputs. sroar
// has no cardinality-only operations, so its count benchmarks run the
// materialize-and-count path that roar's fused operations avoid.

type benchFamily struct {
	name string
	ids  [4][]uint64
}

type benchViews struct {
	views   [4]Bitmap
	bitmaps [4]*sroar.Bitmap
}

var (
	benchOnce sync.Once
	benchData []benchFamily
	benchOpen []benchViews
)

func buildBenchData() {
	benchData = []benchFamily{
		familyFromShapes("geoCells", 100_000, benchTiny),
		familyFromShapes("people", 20_000, benchSmall),
		familyFromShapes("tags", 1_000, benchMedium),
		familyFromShapes("timeBuckets", 60, benchDense),
		familyFromShapes("favorites", 500, benchLarge),
		familyFromShapes("mixed", 3_000, benchMixed),
	}
	benchOpen = make([]benchViews, len(benchData))
	for familyIndex, family := range benchData {
		for listIndex, ids := range family.ids {
			data, err := FromSorted(ids)
			if err != nil {
				panic(err)
			}
			benchOpen[familyIndex].views[listIndex] = OpenTrusted(data)
			benchOpen[familyIndex].bitmaps[listIndex] = sroar.FromSortedList(ids)
		}
	}
}

func allBenchData() ([]benchFamily, []benchViews) {
	benchOnce.Do(buildBenchData)
	return benchData, benchOpen
}

type benchShape uint8

const (
	benchTiny benchShape = iota
	benchSmall
	benchMedium
	benchDense
	benchLarge
	benchMixed
)

func familyFromShapes(name string, poolSize int, shape benchShape) benchFamily {
	rng := rand.New(rand.NewPCG(7, uint64(len(name))+1))
	pool := keyPool(rng, poolSize)
	family := benchFamily{name: name}
	for listIndex := range family.ids {
		ids := make([]uint64, 0, poolSize*64)
		for _, key := range pool {
			if rng.IntN(10) >= 7 && shape != benchDense {
				continue // ~30% of keys absent per list, creating overlap
			}
			appendShapedIDs(rng, shape, key, &ids)
		}
		sortIDs(ids)
		family.ids[listIndex] = ids
	}
	return family
}

func appendShapedIDs(rng *rand.Rand, shape benchShape, key uint64, ids *[]uint64) {
	var values []uint16
	switch shape {
	case benchTiny:
		values = distinctValues(rng, 1+rng.IntN(4))
	case benchSmall:
		values = distinctValues(rng, 5+rng.IntN(295))
	case benchMedium:
		values = distinctValues(rng, 1000+rng.IntN(4000))
	case benchDense:
		start := uint16(rng.IntN(2000))
		values = make([]uint16, 0, 30000+rng.IntN(35000))
		for value := start; value != 0; value++ {
			values = append(values, value)
			if len(values) == cap(values) {
				break
			}
		}
	case benchLarge:
		values = distinctValues(rng, 5000+rng.IntN(10000))
	case benchMixed:
		switch rng.IntN(3) {
		case 0:
			values = distinctValues(rng, 1+rng.IntN(4))
		case 1:
			values = distinctValues(rng, 50+rng.IntN(1950))
		default:
			values = distinctValues(rng, 3000+rng.IntN(17000))
		}
	}
	for _, value := range values {
		*ids = append(*ids, key|uint64(value))
	}
}

func sortIDs(ids []uint64) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

var benchSink uint64
var benchSinkBytes []byte

func BenchmarkBuild(b *testing.B) {
	data, _ := allBenchData()
	for familyIndex := range data {
		family := &data[familyIndex]
		for listIndex := 0; listIndex < 2; listIndex++ {
			ids := family.ids[listIndex]
			b.Run(fmt.Sprintf("%s/%d/Roar", family.name, listIndex), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					result, err := FromSorted(ids)
					if err != nil {
						b.Fatal(err)
					}
					benchSinkBytes = result
				}
				b.ReportMetric(float64(len(benchSinkBytes)), "bytes")
			})
			b.Run(fmt.Sprintf("%s/%d/Sroar", family.name, listIndex), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSinkBytes = sroar.FromSortedList(ids).ToBuffer()
				}
				b.ReportMetric(float64(len(benchSinkBytes)), "bytes")
			})
		}
	}
}

func BenchmarkContains(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		views := &open[familyIndex]
		family := &benchData[familyIndex]
		ids := family.ids[0]
		misses := make([]uint64, len(ids))
		for index, id := range ids {
			misses[index] = id + 1<<16
		}
		b.Run(family.name+"/Roar/Hit", func(b *testing.B) {
			view := views.views[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for index := 0; index < len(ids); index += 64 {
					if view.Contains(ids[index]) {
						sum++
					}
				}
				benchSink = sum
			}
		})
		b.Run(family.name+"/Roar/Miss", func(b *testing.B) {
			view := views.views[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for index := 0; index < len(misses); index += 64 {
					if view.Contains(misses[index]) {
						sum++
					}
				}
				benchSink = sum
			}
		})
		b.Run(family.name+"/Sroar/Hit", func(b *testing.B) {
			bitmap := views.bitmaps[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for index := 0; index < len(ids); index += 64 {
					if bitmap.Contains(ids[index]) {
						sum++
					}
				}
				benchSink = sum
			}
		})
		b.Run(family.name+"/Sroar/Miss", func(b *testing.B) {
			bitmap := views.bitmaps[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for index := 0; index < len(misses); index += 64 {
					if bitmap.Contains(misses[index]) {
						sum++
					}
				}
				benchSink = sum
			}
		})
	}
}

func BenchmarkIterate(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		views := &open[familyIndex]
		b.Run(benchData[familyIndex].name+"/Roar/AppendTo", func(b *testing.B) {
			view := views.views[0]
			buffer := make([]uint64, 0, view.Cardinality())
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for _, id := range view.AppendTo(buffer[:0]) {
					sum += id
				}
				benchSink = sum
			}
		})
		b.Run(benchData[familyIndex].name+"/Roar/Next", func(b *testing.B) {
			view := views.views[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for iterator := view.Iterator(); iterator.Next(); {
					sum += iterator.Value()
				}
				benchSink = sum
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar/Iterator", func(b *testing.B) {
			bitmap := views.bitmaps[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				iterator := bitmap.NewIterator()
				for value := iterator.Next(); value != 0; value = iterator.Next() {
					sum += value
				}
				benchSink = sum
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar/ToArray", func(b *testing.B) {
			bitmap := views.bitmaps[0]
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for _, value := range bitmap.ToArray() {
					sum += value
				}
				benchSink = sum
			}
		})
	}
}

func BenchmarkAndCardinality(b *testing.B) {
	_, open := allBenchData()
	builder := NewBuilder(0)
	for familyIndex := range open {
		views := &open[familyIndex]
		b.Run(benchData[familyIndex].name+"/Roar", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = AndCardinality(a, c)
			}
		})
		b.Run(benchData[familyIndex].name+"/Roar/Materialized", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				builder.Reset()
				AndInto(builder, a, c)
				benchSinkBytes = builder.Compact()
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar", func(b *testing.B) {
			a, c := views.bitmaps[0], views.bitmaps[1]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = uint64(a.Clone().And(c).GetCardinality())
			}
		})
	}
}

func BenchmarkOr(b *testing.B) {
	_, open := allBenchData()
	builder := NewBuilder(0)
	for familyIndex := range open {
		views := &open[familyIndex]
		b.Run(benchData[familyIndex].name+"/Roar/OrCardinality", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = OrCardinality(a, c)
			}
		})
		b.Run(benchData[familyIndex].name+"/Roar/Materialized", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				builder.Reset()
				OrInto(builder, a, c)
				benchSinkBytes = builder.Compact()
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar", func(b *testing.B) {
			a, c := views.bitmaps[0], views.bitmaps[1]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = uint64(a.Clone().Or(c).GetCardinality())
			}
		})
	}
}

func BenchmarkAndNot(b *testing.B) {
	_, open := allBenchData()
	builder := NewBuilder(0)
	for familyIndex := range open {
		views := &open[familyIndex]
		b.Run(benchData[familyIndex].name+"/Roar/AndNotCardinality", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = AndNotCardinality(a, c)
			}
		})
		b.Run(benchData[familyIndex].name+"/Roar/Materialized", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				builder.Reset()
				AndNotInto(builder, a, c)
				benchSinkBytes = builder.Compact()
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar", func(b *testing.B) {
			a, c := views.bitmaps[0], views.bitmaps[1]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = uint64(a.Clone().AndNot(c).GetCardinality())
			}
		})
	}
}

func BenchmarkIntersects(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		views := &open[familyIndex]
		b.Run(benchData[familyIndex].name+"/Roar", func(b *testing.B) {
			a, c := views.views[0], views.views[1]
			b.ReportAllocs()
			for b.Loop() {
				if Intersects(a, c) {
					benchSink++
				}
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar", func(b *testing.B) {
			a, c := views.bitmaps[0], views.bitmaps[1]
			b.ReportAllocs()
			for b.Loop() {
				if a.Intersects(c) {
					benchSink++
				}
			}
		})
	}
}

func BenchmarkAndManyCardinality(b *testing.B) {
	_, open := allBenchData()
	builder := NewBuilder(0)
	for familyIndex := range open {
		views := &open[familyIndex]
		b.Run(benchData[familyIndex].name+"/Roar", func(b *testing.B) {
			a, c, d, e := views.views[0], views.views[1], views.views[2], views.views[3]
			b.ReportAllocs()
			for b.Loop() {
				benchSink = AndManyCardinality(a, c, d, e)
			}
		})
		b.Run(benchData[familyIndex].name+"/Roar/Materialized", func(b *testing.B) {
			a, c, d, e := views.views[0], views.views[1], views.views[2], views.views[3]
			b.ReportAllocs()
			for b.Loop() {
				builder.Reset()
				AndManyInto(builder, a, c, d, e)
				benchSinkBytes = builder.Compact()
			}
		})
		b.Run(benchData[familyIndex].name+"/Sroar", func(b *testing.B) {
			bitmaps := views.bitmaps
			b.ReportAllocs()
			for b.Loop() {
				clones := make([]*sroar.Bitmap, 4)
				for index := range clones {
					clones[index] = bitmaps[index].Clone()
				}
				benchSink = uint64(sroar.FastAnd(clones...).GetCardinality())
			}
		})
	}
}
