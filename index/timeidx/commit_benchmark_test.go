package timeidx

import (
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// Group-commit probes on a persistent index. DurabilitySync is the
// interesting mode: the writer loop coalesces every batch queued while a
// sync is in flight into one group sharing one fsync, while a lone writer
// still pays one fsync per Apply.
func BenchmarkApplyPersistentSyncSingle(b *testing.B) {
	idx, err := NewStore(b.TempDir(), Spec{}, Config{DeltaCapacity: 4096}, DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	batch := NewBatch(1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch.Reset()
		batch.Append(Insert(common.ValueID(i+1), uint64(i)*testQuantum))
		applyWithBackpressure(b, idx, batch)
	}
}

func BenchmarkApplyPersistentSyncParallel(b *testing.B) {
	idx, err := NewStore(b.TempDir(), Spec{}, Config{DeltaCapacity: 4096, WriteQueueCapacity: 256}, DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	var workerID atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		base := common.ValueID(workerID.Add(1) * 1_000_000)
		batch := NewBatch(1)
		var sequence uint64
		for pb.Next() {
			sequence++
			batch.Reset()
			batch.Append(Insert(base+common.ValueID(sequence%1_024), (sequence%65536)*testQuantum))
			applyWithBackpressure(b, idx, batch)
		}
	})
}

func applyWithBackpressure(b *testing.B, index *Index, batch *Batch) {
	b.Helper()
	for {
		err := index.Apply(batch)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrBackpressure) {
			b.Fatal(err)
		}
		runtime.Gosched()
	}
}
