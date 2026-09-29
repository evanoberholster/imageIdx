package vectoridx

import (
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentSyncAddReopen proves the deferred-fsync contract: every Add
// acknowledged under DurabilitySync is durable and survives a reopen, even
// when many writers fsync different segments concurrently.
func TestConcurrentSyncAddReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx")
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	const writers, perWriter = 4, 25
	rng := rand.New(rand.NewSource(84))
	vec := randFloatVec(rng, Dim512)
	var next atomic.Uint64
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < perWriter; n++ {
				id := NodeID(next.Add(1))
				if err := idx.AddFloat32(id, vec); err != nil {
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
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != writers*perWriter {
		t.Fatalf("reopened live count = %d, want %d", got, writers*perWriter)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatal(err)
	}
}
