package timeidx

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func BenchmarkTimeShardSize(b *testing.B) {
	entries := make([]Entry, 100000)
	for n := range entries {
		entries[n] = Entry{Timestamp: uint64(n), ImageID: uint64(n + 1)}
	}
	for _, size := range []int64{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("Bytes=%d", size), func(b *testing.B) {
			i, err := NewMemory(Spec{Entries: entries}, Config{MaxShardBytes: size})
			if err != nil {
				b.Fatal(err)
			}
			defer i.Close()
			batch := NewBatch(64)
			b.ReportAllocs()
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				batch.Reset()
				for n := uint64(0); n < 64; n++ {
					batch.Append(Insert(50000+n, 50000+n+uint64(k%2)))
				}
				if err := i.Apply(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("Search/Bytes=%d", size), func(b *testing.B) {
			i, err := NewMemory(Spec{Entries: entries}, Config{MaxShardBytes: size})
			if err != nil {
				b.Fatal(err)
			}
			defer i.Close()
			dst := make([]uint64, 0, 25000)
			b.ReportAllocs()
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				dst = i.Search(dst[:0], 50000, 75000)
			}
		})
	}
}

func BenchmarkNamespaceParallel(b *testing.B) {
	s, err := NewNamespaceStore(b.TempDir(), Config{}, DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	indexes := make([]*Index, 8)
	for n := range indexes {
		indexes[n], err = s.CreateNamespace(schema.Namespace(n + 1))
		if err != nil {
			b.Fatal(err)
		}
	}
	if err := s.SealStartup(); err != nil {
		b.Fatal(err)
	}
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := sequence.Add(1) - 1
		i := indexes[n%8]
		batch := NewBatch(64)
		for pb.Next() {
			k := sequence.Add(1)
			batch.Reset()
			for id := uint64(0); id < 64; id++ {
				batch.Append(Insert(n*64+id, k))
			}
			if err := i.Apply(batch); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkTimeSelectiveCommit(b *testing.B) {
	i := persistent(b, Config{})
	batch := NewBatch(100000)
	for n := uint64(0); n < 100000; n++ {
		batch.Append(Insert(n, n))
	}
	if err := i.Apply(batch); err != nil {
		b.Fatal(err)
	}
	batch = NewBatch(64)
	var logical, allocated uint64
	b.ReportAllocs()
	b.ResetTimer()
	for k := 0; k < b.N; k++ {
		batch.Reset()
		for n := uint64(0); n < 64; n++ {
			batch.Append(Insert(50000+n, 50000+n+uint64((k+1)%2)))
		}
		if err := i.Apply(batch); err != nil {
			b.Fatal(err)
		}
		st := i.CommitStats()
		logical += st.LogicalBytesWritten
		allocated += st.AllocatedBytes
	}
	b.StopTimer()
	b.ReportMetric(float64(logical)/float64(b.N), "logical-B/op")
	b.ReportMetric(float64(allocated)/float64(b.N), "extent-B/op")
	b.ReportMetric(float64(i.OnDiskBytes()), "file-B")
}

func BenchmarkTimeGrowth(b *testing.B) {
	b.ReportAllocs()
	for k := 0; k < b.N; k++ {
		b.StopTimer()
		dir := filepath.Join(b.TempDir(), "store")
		i, err := NewStore(dir, Spec{Namespace: 1}, Config{MaxFileBytes: 4096, MaxShardBytes: 4096}, DurabilitySync)
		if err != nil {
			b.Fatal(err)
		}
		batch := NewBatch(1000)
		for n := uint64(0); n < 1000; n++ {
			batch.Append(Insert(n, n))
		}
		b.StartTimer()
		if err := i.Apply(batch); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		b.ReportMetric(float64(i.OnDiskBytes()), "file-B")
		i.Close()
		b.StartTimer()
	}
}

func BenchmarkTimeHistoricalShardSize(b *testing.B) {
	entries := make([]Entry, 100000)
	for n := range entries {
		entries[n] = Entry{Timestamp: uint64(n), ImageID: uint64(n + 1)}
	}
	for _, size := range []int64{4 << 10, 16 << 10, 64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("Bytes=%d", size), func(b *testing.B) {
			i, err := NewMemory(Spec{Entries: entries}, Config{MaxShardBytes: size})
			if err != nil {
				b.Fatal(err)
			}
			defer i.Close()
			batch := NewBatch(64)
			b.ReportAllocs()
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				batch.Reset()
				for n := 0; n < 64; n++ {
					batch.Append(Insert(uint64(n+1), uint64((k*7919+n*1543)%100000)))
				}
				if err := i.Apply(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTimeSearchDuringCommit(b *testing.B) {
	entries := make([]Entry, 100000)
	for n := range entries {
		entries[n] = Entry{Timestamp: uint64(n), ImageID: uint64(n + 1)}
	}
	i, err := NewStore(b.TempDir(), Spec{Namespace: 1, Entries: entries}, Config{}, DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer i.Close()
	dst := make([]uint64, 0, 5000)
	samples := make([]int64, 0, min(b.N, 4096))
	stop := make(chan struct{})
	done := make(chan error, 1)
	ready := make(chan struct{})
	var commits atomic.Uint64
	go func() {
		batch := NewBatch(64)
		close(ready)
		for k := 0; ; k++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			batch.Reset()
			for n := uint64(1); n <= 64; n++ {
				batch.Append(Insert(n, 20000+n+uint64(k%2)))
			}
			if err := i.Apply(batch); err != nil {
				done <- err
				return
			}
			commits.Add(1)
		}
	}()
	<-ready
	b.ReportAllocs()
	b.ResetTimer()
	bad := false
	for n := 0; n < b.N; n++ {
		if len(samples) < cap(samples) {
			start := time.Now()
			dst = i.Search(dst[:0], 30000, 35000)
			samples = append(samples, time.Since(start).Nanoseconds())
		} else {
			dst = i.Search(dst[:0], 30000, 35000)
		}
		if len(dst) != 5000 {
			bad = true
			break
		}
	}
	b.StopTimer()
	close(stop)
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	if bad {
		b.Fatal("incoherent result")
	}
	slices.Sort(samples)
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[len(samples)/2]), "p50-ns")
		b.ReportMetric(float64(samples[(len(samples)-1)*99/100]), "p99-ns")
	}
	b.ReportMetric(float64(commits.Load()), "commits")
}
