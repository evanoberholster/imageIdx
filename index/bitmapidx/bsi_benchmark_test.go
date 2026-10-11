package bitmapidx

import (
	"math/rand"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// BSI benchmark distributions. The same shapes are exercised for both widths
// and compared against the equivalent continuous namespace.
type bsiBenchDist int

const (
	bsiDistUniform bsiBenchDist = iota
	bsiDistConcentrated
	bsiDistSequential
	bsiDistClustered
	bsiDistZero
	bsiDistMax
)

func (d bsiBenchDist) String() string {
	switch d {
	case bsiDistUniform:
		return "uniform"
	case bsiDistConcentrated:
		return "concentrated"
	case bsiDistSequential:
		return "sequential"
	case bsiDistClustered:
		return "clustered"
	case bsiDistZero:
		return "zero"
	default:
		return "max"
	}
}

func bsiBenchValues(dist bsiBenchDist, width uint8, n int) []uint32 {
	rng := rand.New(rand.NewSource(0x8511))
	max := uint32(bsiMaxValue(width))
	out := make([]uint32, n)
	for i := range out {
		switch dist {
		case bsiDistUniform:
			out[i] = uint32(rng.Int63n(int64(max))) + 1
		case bsiDistConcentrated:
			out[i] = uint32(1 + rng.Intn(4))
		case bsiDistSequential:
			out[i] = uint32(i) % (max + 1)
		case bsiDistClustered:
			// 64 dense clusters scattered across the domain.
			cluster := uint32(rng.Intn(64))
			out[i] = (cluster*(max/64) + uint32(rng.Intn(64))) % (max + 1)
		case bsiDistZero:
			out[i] = 0
		default:
			out[i] = max
		}
	}
	return out
}

// buildBSIBench fills an in-memory BSI namespace with n values and returns it
// with the ids, ready for read benchmarks.
func buildBSIBench(b *testing.B, width uint8, dist bsiBenchDist, n int) (*Index, []common.ValueID) {
	b.Helper()
	index, err := NewMemory(bsiNamespace(testBSI16Namespace, width), NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	values := bsiBenchValues(dist, width, n)
	ids := make([]common.ValueID, n)
	batch := index.NewBatch()
	for i, value := range values {
		id := common.ValueID(i + 1)
		ids[i] = id
		if err := batch.BSIMove(BSIMove{ID: id, To: value, HasTo: true}); err != nil {
			b.Fatal(err)
		}
		if batch.Len() >= 4096 {
			_ = batch.Apply()
			_ = index.Barrier()
			batch.Reset()
		}
	}
	if !batch.Empty() {
		_ = batch.Apply()
		_ = index.Barrier()
	}
	return index, ids
}

// fillBSIBench populates a BSI namespace with batched writes so benchmark
// setup does not pay one WAL sync per value.
func fillBSIBench(b *testing.B, index *Index, values []uint32) {
	b.Helper()
	batch := index.NewBatch()
	for i, value := range values {
		if err := batch.BSIMove(BSIMove{ID: common.ValueID(i + 1), To: value, HasTo: true}); err != nil {
			b.Fatal(err)
		}
		if batch.Len() >= 4096 {
			if err := batch.Apply(); err != nil {
				b.Fatal(err)
			}
			if err := index.Barrier(); err != nil {
				b.Fatal(err)
			}
			_ = batch.Reset()
		}
	}
	if !batch.Empty() {
		_ = batch.Apply()
		_ = index.Barrier()
	}
}

func benchBSIRangeRead(b *testing.B, width uint8, size int, lo, hi uint64) {
	index, _ := buildBSIBench(b, width, bsiDistUniform, size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := index.BSIRange(lo, hi); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBSIEqual(b *testing.B) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		for _, size := range []int{10_000, 100_000} {
			b.Run(sizeName(width, size), func(b *testing.B) {
				index, _ := buildBSIBench(b, width, bsiDistUniform, size)
				probe := bsiMaxValue(width) / 3
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := index.Equal(probe); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkBSIRange(b *testing.B) {
	max16 := bsiMaxValue(BSIWidth16)
	max32 := bsiMaxValue(BSIWidth32)
	cases := []struct {
		name   string
		width  uint8
		lo, hi uint64
	}{
		{"narrow", BSIWidth16, max16 / 2, max16/2 + max16/100},
		{"wide", BSIWidth16, max16 / 8, max16 * 7 / 8},
		{"full", BSIWidth16, 0, max16},
		{"narrow", BSIWidth32, max32 / 2, max32/2 + max32/100},
		{"wide", BSIWidth32, max32 / 8, max32 * 7 / 8},
		{"full", BSIWidth32, 0, max32},
	}
	for _, size := range []int{10_000, 100_000} {
		for _, tc := range cases {
			b.Run(tc.name+"-"+sizeName(tc.width, size), func(b *testing.B) {
				benchBSIRangeRead(b, tc.width, size, tc.lo, tc.hi)
			})
		}
	}
}

func BenchmarkBSIFilterRange(b *testing.B) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		for _, size := range []int{10_000, 100_000} {
			index, ids := buildBSIBench(b, width, bsiDistUniform, size)
			max := bsiMaxValue(width)
			for _, count := range []int{1, 8, 32, 64, 256, 1024, 10_000, 100_000} {
				if count > len(ids) {
					continue
				}
				b.Run(countName(width, size, count), func(b *testing.B) {
					candidates := ids[:count]
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := index.BSIFilterRange(nil, candidates, ValuesBetween(max/4, max*3/4)); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func BenchmarkBSISet(b *testing.B) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		b.Run(sizeName(width, 0), func(b *testing.B) {
			index, err := NewMemory(bsiNamespace(testBSI16Namespace, width), NamespaceConfig{})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = index.Close() })
			// Pre-insert ids so the timed loop measures replacement.
			for i := 1; i <= 4096; i++ {
				_ = index.Set(common.ValueID(i), uint64(i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = index.Set(common.ValueID(i%4096+1), uint64(i+1))
			}
		})
	}
}

func BenchmarkBSIBatch64(b *testing.B) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		b.Run(sizeName(width, 0), func(b *testing.B) {
			index, err := NewMemory(bsiNamespace(testBSI16Namespace, width), NamespaceConfig{})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = index.Close() })
			batch := index.NewBatch()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				batch.Reset()
				for j := 0; j < 64; j++ {
					id := common.ValueID((i*64+j)%100_000 + 1)
					if err := batch.Add(Entry{ImageID: id, Value: Uint32Value(uint32((i + j) & 0xFFFF))}); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				_ = batch.Apply()
				_ = index.Barrier()
				b.StartTimer()
			}
		})
	}
}

func BenchmarkBSIDelete(b *testing.B) {
	index, ids := buildBSIBench(b, BSIWidth32, bsiDistUniform, 100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = index.Delete(ids[i%len(ids)])
	}
}

func BenchmarkBSIPersistent(b *testing.B) {
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		for _, size := range []int{10_000, 100_000} {
			b.Run(sizeName(width, size), func(b *testing.B) {
				dir := b.TempDir()
				store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI16Namespace, width)}, NamespaceConfig{}, common.DurabilityOS)
				if err != nil {
					b.Fatal(err)
				}
				index, _ := store.Lookup(testBSI16Namespace)
				values := bsiBenchValues(bsiDistUniform, width, size)
				fillBSIBench(b, index, values)
				_ = store.Checkpoint()
				_ = store.Close()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					reopened, err := OpenStore(dir, NamespaceConfig{}, common.DurabilityOS)
					if err != nil {
						b.Fatal(err)
					}
					// A query forces lazy structures to be touched.
					got, _ := reopened.Lookup(testBSI16Namespace)
					_, _ = got.BSIRange(0, bsiMaxValue(width)/2)
					_ = reopened.Close()
				}
			})
		}
	}
}

func BenchmarkBSICompact(b *testing.B) {
	const n = 100_000
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		b.Run(sizeName(width, n), func(b *testing.B) {
			index, ids := buildBSIBench(b, width, bsiDistUniform, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				batch := index.NewBatch()
				for j := 0; j < 4096; j++ {
					id := ids[(i*4096+j)%n]
					if err := batch.Add(Entry{ImageID: id, Value: Uint32Value(uint32(j))}); err != nil {
						b.Fatal(err)
					}
				}
				_ = batch.Apply()
				_ = index.Barrier()
				b.StartTimer()
				if err := index.Compact(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBSICheckpoint(b *testing.B) {
	const n = 100_000
	for _, width := range []uint8{BSIWidth16, BSIWidth32} {
		b.Run(sizeName(width, n), func(b *testing.B) {
			dir := b.TempDir()
			store, err := NewStore(dir, []NamespaceSpec{bsiNamespace(testBSI16Namespace, width)}, NamespaceConfig{}, common.DurabilityOS)
			if err != nil {
				b.Fatal(err)
			}
			index, _ := store.Lookup(testBSI16Namespace)
			values := bsiBenchValues(bsiDistUniform, width, n)
			fillBSIBench(b, index, values)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.Checkpoint(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			_ = store.Close()
		})
	}
}

func BenchmarkBSIVsContinuousRange(b *testing.B) {
	const size = 100_000
	values := bsiBenchValues(bsiDistUniform, BSIWidth16, size)
	max := bsiMaxValue(BSIWidth16)

	b.Run("bsi16", func(b *testing.B) {
		index, _ := buildBSIBench(b, BSIWidth16, bsiDistUniform, size)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = index.BSIRange(max/8, max*7/8)
		}
	})
	b.Run("continuous", func(b *testing.B) {
		index, err := NewMemory(continuousNamespace(testFocalLength), NamespaceConfig{})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = index.Close() })
		batch := index.NewBatch()
		for i, value := range values {
			_ = batch.Add(Entry{ImageID: common.ImageID(i + 1), Value: IntValue(int64(value))})
			if batch.Len() >= 4096 {
				_ = batch.Apply()
				_ = index.Barrier()
				batch.Reset()
			}
		}
		_ = batch.Apply()
		_ = index.Barrier()
		bounds := Between(int64(max/8), int64(max*7/8))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = index.Range(bounds)
		}
	})
}

func sizeName(width uint8, size int) string {
	if width == BSIWidth16 {
		if size == 0 {
			return "bsi16"
		}
		return "bsi16-n" + itoa(size)
	}
	if size == 0 {
		return "bsi32"
	}
	return "bsi32-n" + itoa(size)
}

func countName(width uint8, size, count int) string {
	return sizeName(width, size) + "-c" + itoa(count)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
