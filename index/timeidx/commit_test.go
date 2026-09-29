package timeidx

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// TestConcurrentSyncApplyReopen proves the writer-loop group contract: every
// Apply that returned nil under DurabilitySync is durable and replays after a
// reopen, even when many writers commit in shared groups.
func TestConcurrentSyncApplyReopen(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewStore(dir, Spec{}, Config{DeltaCapacity: 4096}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	const writers, perWriter = 4, 25
	var next atomic.Uint64
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := NewBatch(1)
			for n := 0; n < perWriter; n++ {
				id := common.ValueID(next.Add(1))
				batch.Reset()
				batch.Append(Insert(id, uint64(id)*testQuantum))
				for {
					err := idx.Apply(batch)
					if err == nil {
						break
					}
					if !errors.Is(err, ErrBackpressure) {
						t.Error(err)
						return
					}
					runtime.Gosched()
				}
			}
		}()
	}
	wg.Wait()
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, Config{DeltaCapacity: 4096}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got := reopened.Search(nil, 0, ^uint64(0))
	if len(got) != writers*perWriter {
		t.Fatalf("reopened search returned %d ids, want %d", len(got), writers*perWriter)
	}
}
