package timestamp

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
)

func BenchmarkSearch(b *testing.B) {
	benchmarkSearchSize(b, 1_000_000)
}

func BenchmarkSearchParallel(b *testing.B) {
	index, end := benchmarkIndex(b, 1_000_000, Config{})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buffer := make([]ImageID, 0, 4_096)
		for pb.Next() {
			buffer = index.Search(buffer[:0], end/2, end/2+1_024*testQuantum)
		}
	})
}

func BenchmarkSearchLarge(b *testing.B) {
	if os.Getenv("TIMESTAMP_BENCH_LARGE") == "" {
		b.Skip("set TIMESTAMP_BENCH_LARGE=1 to build the 10M and 100M synthetic indexes")
	}
	for _, size := range []int{10_000_000, 100_000_000} {
		b.Run(fmt.Sprintf("Entries-%d", size), func(b *testing.B) {
			benchmarkSearchSize(b, size)
		})
	}
}

func benchmarkSearchSize(b *testing.B, size int) {
	index, end := benchmarkIndex(b, size, Config{})
	ranges := []struct {
		name  string
		width uint64
		cap   int
	}{
		{name: "Narrow", width: 16, cap: 64},
		{name: "Medium", width: 4_096, cap: 16_384},
		{name: "Broad", width: uint64(size / 4), cap: size},
	}
	for _, test := range ranges {
		b.Run(test.name, func(b *testing.B) {
			buffer := make([]ImageID, 0, test.cap)
			start := end / 2
			finish := start + test.width*testQuantum
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				buffer = index.Search(buffer[:0], start, finish)
			}
		})
	}
}

func BenchmarkUpdate(b *testing.B) {
	index, _ := benchmarkIndex(b, 65_536, Config{DeltaCapacity: 4_096})
	batch := NewWriteBatch(1)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		batch.Reset()
		batch.Append(TimestampWrite{
			Timestamp: uint64(iteration%65_536) * testQuantum,
			ImageID:   ImageID(iteration%65_536 + 1),
			Op:        WriteAdd,
		})
		submitWithBackpressure(index, batch)
	}
}

func BenchmarkUpdateParallel(b *testing.B) {
	index, _ := benchmarkIndex(b, 65_536, Config{DeltaCapacity: 4_096, WriteQueueCapacity: 256})
	var workerID atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		idBase := ImageID(workerID.Add(1) * 1_000_000)
		batch := NewWriteBatch(1)
		var sequence uint64
		for pb.Next() {
			sequence++
			batch.Reset()
			batch.Append(TimestampWrite{
				Timestamp: (sequence % 65_536) * testQuantum,
				ImageID:   idBase + ImageID(sequence%1_024),
				Op:        WriteAdd,
			})
			submitWithBackpressure(index, batch)
		}
	})
}

func BenchmarkDeltaCapacity(b *testing.B) {
	for _, capacity := range []int{2_048, 4_096} {
		b.Run(fmt.Sprintf("Capacity-%d", capacity), func(b *testing.B) {
			index, _ := benchmarkIndex(b, 65_536, Config{DeltaCapacity: capacity})
			batch := NewWriteBatch(1)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				batch.Reset()
				batch.Append(TimestampWrite{
					Timestamp: uint64(iteration%65_536) * testQuantum,
					ImageID:   ImageID(iteration%65_536 + 1),
					Op:        WriteAdd,
				})
				submitWithBackpressure(index, batch)
			}
		})
	}
}

func BenchmarkBlockSize(b *testing.B) {
	for _, size := range []int{32_768, 65_536, 131_072} {
		b.Run(fmt.Sprintf("Entries-%d", size), func(b *testing.B) {
			index, end := benchmarkIndex(b, 1_000_000, Config{BlockSize: size})
			buffer := make([]ImageID, 0, 16_384)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				buffer = index.Search(buffer[:0], end/2, end/2+4_096*testQuantum)
			}
		})
	}
}

func benchmarkIndex(b *testing.B, count int, config Config) (*Index, uint64) {
	b.Helper()
	records := make([]Record, count)
	for index := range records {
		records[index] = Record{
			Timestamp: uint64(index) * testQuantum,
			ImageID:   ImageID(index + 1),
		}
	}
	idx, err := New(records, 0, config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })
	return idx, uint64(count) * testQuantum
}

func submitWithBackpressure(index *Index, batch *TimestampWriteBatch) {
	for {
		err := index.Submit(batch)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrBackpressure) {
			panic(err)
		}
		runtime.Gosched()
	}
}
