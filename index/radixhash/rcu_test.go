package radixhash

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSparseRootLeafThenPromotion verifies that a small bucket is a direct
// Root -> Leaf and is promoted to Root -> Radix -> Leaf once it grows.
func TestSparseRootLeafThenPromotion(t *testing.T) {
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 2048})
	defer index.Close()

	for b := 0; b < 16; b++ {
		h := hashWithPrefix([]byte{byte(b), 0xAA}, uint64(b)+1)
		if err := index.Apply([]Entry{{Hash: h, ImageID: ImageID(b + 1)}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if depth := index.Metadata().Depth; depth != 1 {
		t.Fatalf("sparse depth = %d, want 1", depth)
	}
	if _, isNode := decodeChild(index.view.Load().root[0]); isNode {
		t.Fatalf("root bucket 0 is a radix node before promotion, want a leaf")
	}

	var entries []Entry
	for i := 0; i < 300; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{0, 0xAA}, uint64(i+2)), ImageID: ImageID(1000 + i)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("promoting Apply: %v", err)
	}
	if depth := index.Metadata().Depth; depth < 2 {
		t.Fatalf("depth after promotion = %d, want >= 2", depth)
	}
	if _, isNode := decodeChild(index.view.Load().root[0]); !isNode {
		t.Fatalf("root bucket 0 is still a leaf after promotion")
	}
	if got := index.Lookup(nil, hashWithPrefix([]byte{0, 0xAA}, 1)); !equalIDs(got, []ImageID{1}) {
		t.Fatalf("pre-promotion entry = %v, want [1]", got)
	}
	if got := index.Lookup(nil, hashWithPrefix([]byte{0, 0xAA}, 7)); !equalIDs(got, []ImageID{1005}) {
		t.Fatalf("post-promotion entry = %v, want [1005]", got)
	}
	if got := index.Lookup(nil, hashWithPrefix([]byte{5, 0xAA}, 6)); !equalIDs(got, []ImageID{6}) {
		t.Fatalf("untouched sparse bucket = %v, want [6]", got)
	}
}

// TestConcurrentLookupCompact races readers against arena replacement.
func TestConcurrentLookupCompact(t *testing.T) {
	index := newTestIndex(t, 16<<20, Config{PromotionBytes: 4096})
	defer index.Close()

	const n = 200
	hashes := make([]Hash, n)
	entries := make([]Entry, n)
	for i := 0; i < n; i++ {
		hashes[i] = hashWithPrefix([]byte{byte(i), byte(i >> 4)}, uint64(i)+1)
		entries[i] = Entry{Hash: hashes[i], ImageID: ImageID(i + 1)}
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var dst []ImageID
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, h := range hashes {
					dst = index.Lookup(dst[:0], h)
				}
			}
		}()
	}

	var compactErr error
	var once sync.Once
	for c := 0; c < 10; c++ {
		if err := index.Compact(); err != nil {
			once.Do(func() { compactErr = err })
			break
		}
	}
	close(stop)
	readers.Wait()
	if compactErr != nil {
		t.Fatalf("Compact: %v", compactErr)
	}
	for i := 0; i < n; i++ {
		if got := index.Lookup(nil, hashes[i]); !equalIDs(got, []ImageID{ImageID(i + 1)}) {
			t.Fatalf("Lookup(%d) after compact = %v", i, got)
		}
	}
}

// TestConcurrentLookupClose races readers against Close on a file-backed arena,
// so Close actually unmaps the memory the readers use.
func TestConcurrentLookupClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	index, err := NewMmap(path, 8<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}

	const n = 64
	hashes := make([]Hash, n)
	entries := make([]Entry, n)
	for i := 0; i < n; i++ {
		hashes[i] = hashWithPrefix([]byte{byte(i), byte(i >> 3)}, uint64(i)+1)
		entries[i] = Entry{Hash: hashes[i], ImageID: ImageID(i + 1)}
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var dst []ImageID
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, h := range hashes {
					dst = index.Lookup(dst[:0], h)
				}
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(stop)
	readers.Wait()

	// Reads after Close are safe, skip the released arena, and return empty.
	if got := index.Lookup(nil, hashes[0]); len(got) != 0 {
		t.Fatalf("Lookup after Close = %v, want empty", got)
	}
}

// TestReclaimWithActiveReaders reclaims retired extents while readers run.
func TestReclaimWithActiveReaders(t *testing.T) {
	index := newTestIndex(t, 8<<20, Config{})
	defer index.Close()
	h := hashWithPrefix([]byte{9, 9}, 9)
	for id := ImageID(1); id <= 200; id++ {
		if err := index.Apply([]Entry{{Hash: h, ImageID: id}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if index.Metadata().RetiredExtents == 0 {
		t.Fatal("expected retired extents before reclaim")
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var dst []ImageID
			for {
				select {
				case <-stop:
					return
				default:
				}
				dst = index.Lookup(dst[:0], h)
			}
		}()
	}
	for c := 0; c < 5; c++ {
		if err := index.Reclaim(); err != nil {
			t.Fatalf("Reclaim: %v", err)
		}
	}
	close(stop)
	readers.Wait()
	if got := index.Count(h); got != 200 {
		t.Fatalf("Count = %d, want 200", got)
	}
}

// TestDuplicatesAcrossPromotion verifies duplicate ImageIDs survive a leaf
// promotion and that re-adding an existing pair stays idempotent.
func TestDuplicatesAcrossPromotion(t *testing.T) {
	index := newTestIndex(t, testArenaBytes, Config{PromotionBytes: 1024})
	defer index.Close()
	h := hashWithPrefix([]byte{2, 2}, 42)
	for id := ImageID(1); id <= 5; id++ {
		if err := index.Apply([]Entry{{Hash: h, ImageID: id}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	var entries []Entry
	for i := 0; i < 200; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{2, 2}, uint64(100+i)), ImageID: ImageID(1000 + i)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("promoting Apply: %v", err)
	}
	if depth := index.Metadata().Depth; depth < 2 {
		t.Fatalf("depth = %d, want >= 2", depth)
	}
	if got := index.Lookup(nil, h); !equalIDs(got, []ImageID{1, 2, 3, 4, 5}) {
		t.Fatalf("duplicates after promotion = %v, want [1 2 3 4 5]", got)
	}
	if err := index.Apply([]Entry{{Hash: h, ImageID: 3}}); err != nil {
		t.Fatalf("duplicate re-add: %v", err)
	}
	if got := index.Count(h); got != 5 {
		t.Fatalf("Count after duplicate re-add = %d, want 5", got)
	}
}

// TestLookupCloseStress repeatedly races mmap Lookup against Close. Close
// unmaps the arena, so a reader that registered after the grace period would
// fault; the strict Acquire -> check closed -> use -> Release order prevents
// that. Run with -race.
func TestLookupCloseStress(t *testing.T) {
	dir := t.TempDir()
	for iter := 0; iter < 25; iter++ {
		path := filepath.Join(dir, fmt.Sprintf("idx-%d.bin", iter))
		index, err := NewMmap(path, 2<<20, Config{})
		if err != nil {
			t.Fatalf("NewMmap: %v", err)
		}
		const n = 16
		hashes := make([]Hash, n)
		entries := make([]Entry, n)
		for i := 0; i < n; i++ {
			hashes[i] = hashWithPrefix([]byte{byte(i), byte(iter)}, uint64(i)+1)
			entries[i] = Entry{Hash: hashes[i], ImageID: ImageID(i + 1)}
		}
		if err := index.Apply(entries); err != nil {
			t.Fatalf("Apply: %v", err)
		}

		var readers sync.WaitGroup
		for r := 0; r < 4; r++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				var dst []ImageID
				for j := 0; j < 500; j++ {
					for _, h := range hashes {
						dst = index.Lookup(dst[:0], h)
					}
				}
			}()
		}
		runtime.Gosched()
		if err := index.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		readers.Wait()
	}
}
