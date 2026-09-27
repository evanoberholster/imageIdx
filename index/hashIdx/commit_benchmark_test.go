package hashidx

import (
	common "github.com/evanoberholster/imageIdx/common"
	"sync/atomic"
	"testing"
)

// Group-commit probes. common.DurabilitySync is the interesting mode: the commit
// loop coalesces every request queued while a sync is in flight into one
// group sharing one fsync, while a lone writer still pays one fsync per
// Apply.
func BenchmarkApplySyncSingle(b *testing.B) {
	idx, err := newIndex(b.TempDir(), testPerceptualSpec(Config{}), common.DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := next.Add(1)
		if err := idx.Apply([]Entry{{ImageID: common.ImageID(id), PHash64: id, PDQ: pdqFrom(id)}}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "entries/sec")
}

func BenchmarkApplySyncParallel(b *testing.B) {
	idx, err := newIndex(b.TempDir(), testPerceptualSpec(Config{}), common.DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := next.Add(1)
			if err := idx.Apply([]Entry{{ImageID: common.ImageID(id), PHash64: id, PDQ: pdqFrom(id)}}); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "entries/sec")
}
