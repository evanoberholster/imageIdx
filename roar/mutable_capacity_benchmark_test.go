package roar

import (
	"fmt"
	"testing"
)

// inlineDeltaCapacity is a compile-time const, so it cannot be swept at
// runtime. These benchmarks model its effect: construction zeroes 2*cap*8
// bytes (the two [cap]uint64 fields), and any delta exceeding cap forces a
// heap reallocation of the pending slice.
//
// The model allocates the two backing buffers as separate make calls rather
// than one struct, so allocs/op overstates the real single struct allocation,
// but the zeroing cost and the append-growth crossover that decide the ideal
// size are faithfully reproduced.

var capacitySweep = []int{8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096}

type deltaWorkload struct {
	name    string
	adds    int
	removes int
}

var deltaWorkloads = []deltaWorkload{
	{"tiny_8_4", 8, 4},
	{"small_32_16", 32, 16},
	{"medium_256_128", 256, 128},
	{"large_2048_1024", 2048, 1024},
	{"max_4095_2048", 4095, 2048},
}

// BenchmarkInlineBufferConstruct models the allocation+zeroing of both inline
// buffers as inlineDeltaCapacity grows.
func BenchmarkInlineBufferConstruct(b *testing.B) {
	for _, cap := range capacitySweep {
		b.Run(fmt.Sprintf("cap=%d", cap), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				adds := make([]uint64, 0, cap)
				removes := make([]uint64, 0, cap)
				_, _ = adds, removes
			}
		})
	}
}

// BenchmarkInlineBufferMutate models construction plus a delta of the given
// size at each capacity. Appending beyond cap reallocates, so the combined
// construction cost and append growth reveal the crossover per workload.
func BenchmarkInlineBufferMutate(b *testing.B) {
	for _, work := range deltaWorkloads {
		for _, cap := range capacitySweep {
			b.Run(fmt.Sprintf("%s/cap=%d", work.name, cap), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					adds := make([]uint64, 0, cap)
					removes := make([]uint64, 0, cap)
					for i := 0; i < work.adds; i++ {
						adds = append(adds, uint64(i+1))
					}
					for i := 0; i < work.removes; i++ {
						removes = append(removes, uint64(i+1))
					}
					_ = adds
					_ = removes
				}
			})
		}
	}
}

// BenchmarkMutableBuilderRealSweep measures the real MutableBuilder (current
// inlineDeltaCapacity) end-to-end across delta sizes, as ground truth.
func BenchmarkMutableBuilderRealSweep(b *testing.B) {
	for _, work := range deltaWorkloads {
		b.Run(work.name, func(b *testing.B) {
			adds := make([]uint64, work.adds)
			removes := make([]uint64, work.removes)
			for i := range adds {
				adds[i] = uint64(i + 1)
			}
			for i := range removes {
				removes[i] = uint64(1<<20 + i + 1)
			}
			b.ReportAllocs()
			for range b.N {
				builder := NewMutableBuilder()
				builder.AddMany(adds)
				for _, id := range removes {
					builder.Remove(id)
				}
				_ = builder.Freeze()
			}
		})
	}
}
