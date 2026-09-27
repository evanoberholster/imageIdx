package hashidx

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentSyncAppliesReopen proves the group-commit contract: every
// Apply that returned nil under DurabilitySync is durable and survives a
// reopen, even when many writers commit in shared groups.
func TestConcurrentSyncAppliesReopen(t *testing.T) {
	dir := t.TempDir()
	idx, err := newIndex(dir, testPerceptualSpec(Config{}), DurabilitySync)
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
			for n := 0; n < perWriter; n++ {
				id := ImageID(next.Add(1))
				if err := idx.Apply([]Entry{{ImageID: id, PHash64: uint64(id), PDQ: pdqFrom(uint64(id))}}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openIndex(dir, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Metadata().RecordCount; got != writers*perWriter {
		t.Fatalf("reopened record count = %d, want %d", got, writers*perWriter)
	}
}
