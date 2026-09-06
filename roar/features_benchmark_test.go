package roar

import (
	"math/rand/v2"
	"strconv"
	"testing"
)

// Benchmarks for the features added after the core representation:
// serialization (#4), rank/select/min/max (#5), direct set ops and range
// iterators (#6), the membership cursor (#2), and concurrent set ops (#7).

func BenchmarkMarshalBinary(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		b.Run(benchData[familyIndex].name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				data, _ := bitmap.MarshalBinary()
				benchSinkBytes = data
			}
		})
	}
}

func BenchmarkClone(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		b.Run(benchData[familyIndex].name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				clone := bitmap.Clone()
				benchSink += uint64(len(clone.data))
			}
		})
	}
}

func BenchmarkRank(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		ids := benchData[familyIndex].ids[0]
		probes := []uint64{ids[len(ids)/4], ids[len(ids)/2], ids[3*len(ids)/4], ids[len(ids)-1]}
		b.Run(benchData[familyIndex].name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for _, probe := range probes {
					sum += bitmap.Rank(probe)
				}
				benchSink = sum
			}
		})
	}
}

func BenchmarkSelect(b *testing.B) {
	_, open := allBenchData()
	rng := rand.New(rand.NewPCG(3, 4))
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		card := bitmap.Cardinality()
		b.Run(benchData[familyIndex].name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for range 4 {
					value, _ := bitmap.Select(uint64(rng.Int64N(int64(card))))
					sum += value
				}
				benchSink = sum
			}
		})
	}
}

func BenchmarkMinMax(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		b.Run(benchData[familyIndex].name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				min, _ := bitmap.Minimum()
				max, _ := bitmap.Maximum()
				benchSink = min + max
			}
		})
	}
}

func BenchmarkCursorContains(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		ids := benchData[familyIndex].ids[0]
		b.Run(benchData[familyIndex].name+"/Forward", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				cursor := bitmap.NewCursor()
				count := uint64(0)
				for index := 0; index < len(ids); index += 16 {
					if cursor.Contains(ids[index]) {
						count++
					}
				}
				benchSink = count
			}
		})
		b.Run(benchData[familyIndex].name+"/Contains", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				count := uint64(0)
				for index := 0; index < len(ids); index += 16 {
					if bitmap.Contains(ids[index]) {
						count++
					}
				}
				benchSink = count
			}
		})
	}
}

func BenchmarkCursorNextGeq(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		bitmap := open[familyIndex].views[0]
		ids := benchData[familyIndex].ids[0]
		b.Run(benchData[familyIndex].name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				cursor := bitmap.NewCursor()
				probe := uint64(0)
				count := uint64(0)
				for range 256 {
					value, ok := cursor.NextGeq(probe)
					if !ok {
						break
					}
					probe = value + 1
					count++
				}
				benchSink = count + uint64(len(ids))
			}
		})
	}
}

func BenchmarkDirectSetOps(b *testing.B) {
	_, open := allBenchData()
	for familyIndex := range open {
		views := &open[familyIndex]
		a, c := views.views[0], views.views[1]
		b.Run(benchData[familyIndex].name+"/And", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				result := And(a, c)
				benchSink += result.Cardinality()
			}
		})
		b.Run(benchData[familyIndex].name+"/Or", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				result := Or(a, c)
				benchSink += result.Cardinality()
			}
		})
		b.Run(benchData[familyIndex].name+"/AndNot", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				result := AndNot(a, c)
				benchSink += result.Cardinality()
			}
		})
	}
}

func BenchmarkConcurrentSetOps(b *testing.B) {
	_, open := allBenchData()
	for _, familyIndex := range []int{0, 3, 5} { // geoCells, timeBuckets, mixed
		views := &open[familyIndex]
		a, c := views.views[0], views.views[1]
		for _, concurrency := range []int{4, 8} {
			b.Run(benchData[familyIndex].name+"/And/seq", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink += And(a, c).Cardinality()
				}
			})
			b.Run(benchData[familyIndex].name+"/AndConc/"+strconv.Itoa(concurrency), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink += AndConc(a, c, concurrency).Cardinality()
				}
			})
			b.Run(benchData[familyIndex].name+"/OrConc/"+strconv.Itoa(concurrency), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink += OrConc(a, c, concurrency).Cardinality()
				}
			})
		}
	}
}

func BenchmarkFastParOr(b *testing.B) {
	_, open := allBenchData()
	for _, familyIndex := range []int{0, 5} {
		views := &open[familyIndex]
		bitmaps := views.views[:]
		b.Run(benchData[familyIndex].name+"/seq", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				folded := Or(Or(bitmaps[0], bitmaps[1]), Or(bitmaps[2], bitmaps[3]))
				benchSink += folded.Cardinality()
			}
		})
		for _, numGo := range []int{4, 8} {
			b.Run(benchData[familyIndex].name+"/FastParOr/"+strconv.Itoa(numGo), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink += FastParOr(numGo, bitmaps...).Cardinality()
				}
			})
		}
	}
}

func BenchmarkRangeIterators(b *testing.B) {
	_, open := allBenchData()
	for _, familyIndex := range []int{0, 5} {
		bitmap := open[familyIndex].views[0]
		b.Run(benchData[familyIndex].name+"/AppendTo", func(b *testing.B) {
			b.ReportAllocs()
			buffer := make([]uint64, 0, bitmap.Cardinality())
			for b.Loop() {
				sum := uint64(0)
				for _, id := range bitmap.AppendTo(buffer[:0]) {
					sum += id
				}
				benchSink = sum
			}
		})
		b.Run(benchData[familyIndex].name+"/Range8", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sum := uint64(0)
				for _, iterator := range bitmap.NewRangeIterators(8) {
					for iterator.Next() {
						sum += iterator.Value()
					}
				}
				benchSink = sum
			}
		})
	}
}
