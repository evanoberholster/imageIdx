package timeidx

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

func extentCapacity(es []storage.Extent) uint64 {
	var bytes uint64
	for _, e := range es {
		bytes += storage.Align(uint64(e.Size), 512)
	}
	return bytes
}

// Checks both recoverable generations, not just the current reader view.
// Caller is the only writer, between completed commits. Duplicate IDs, ordering, CRCs, bounds and
// within-generation overlaps are checked by the normal recovery decoder;
// allocator reconstruction additionally checks the cross-generation union.
func checkGrowthState(t *testing.T, i *Index, entries []Entry, present []bool) {
	t.Helper()
	expected := make([]Entry, 0, len(entries))
	for n, e := range entries {
		if present[n] {
			expected = append(expected, e)
		}
	}
	slices.SortFunc(expected, func(a, b Entry) int {
		if d := cmp.Compare(a.Timestamp, b.Timestamp); d != 0 {
			return d
		}
		return cmp.Compare(a.ImageID, b.ImageID)
	})
	got := i.Search(nil, 0, ^uint64(0))
	if len(got) != len(expected) || i.Len() != uint64(len(expected)) {
		t.Fatal("cardinality", len(got), len(expected))
	}
	for n, e := range expected {
		if got[n] != e.ImageID {
			t.Fatal("incorrect committed records", n)
		}
	}
	pos := 0
	for _, s := range i.view.Load().shards {
		for n, ts := range s.times {
			if ts != expected[pos].Timestamp || s.ids[n] != expected[pos].ImageID {
				t.Fatal("incorrect timestamp snapshot", pos)
			}
			pos++
		}
	}
	gs, err := recoverGenerations(i.file.Bytes(), i.namespace)
	if err != nil || len(gs) != 2 {
		t.Fatal("lost recoverable generation", len(gs), err)
	}
	if gs[0].view.generation != i.Generation() || gs[0].view.sourceGeneration != i.SourceGeneration() {
		t.Fatal("watermark/root mismatch")
	}
	if gs[1].view.sourceGeneration > gs[0].view.sourceGeneration {
		t.Fatal("decreasing watermark")
	}
	union, err := storage.UnionExtents(gs[0].live, gs[1].live)
	if err != nil {
		t.Fatal(err)
	}
	a, err := storage.ReconstructAllocator(uint64(len(i.file.Bytes())), union)
	if err != nil {
		t.Fatal("overlapping recoverable extents", err)
	}
	slices.SortFunc(gs[0].live, func(a, b storage.Extent) int { return cmp.Compare(a.Ref, b.Ref) })
	if a.FreeBytes() != i.alloc.FreeBytes() || !slices.Equal(union, i.live) || !slices.Equal(gs[0].live, i.currentLive) {
		t.Fatal("allocator retention mismatch")
	}
	if len(i.retired) != 0 {
		t.Fatal("undrained mapping")
	}
}

func TestSustainedGrowthAndExtentReuse(t *testing.T) {
	for _, random := range []bool{false, true} {
		t.Run(fmt.Sprintf("Random=%t", random), func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{MaxFileBytes: 4096, MaxShardBytes: 512}
			i, err := NewStore(dir, Spec{Namespace: 1}, cfg, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { i.Close() }()
			entries := make([]Entry, 2112)
			present := make([]bool, len(entries))
			for n := range entries {
				entries[n] = Entry{Timestamp: uint64(n+1) * 1000, ImageID: scaleID(n, random)}
			}
			var readers sync.WaitGroup
			stop := make(chan struct{})
			ready := make(chan struct{}, 4)
			startReaders := func() {
				for n := 0; n < 4; n++ {
					readers.Add(1)
					go func(idx *Index) {
						defer readers.Done()
						dst := make([]uint64, 0, len(entries))
						ready <- struct{}{}
						for {
							select {
							case <-stop:
								return
							default:
							}
							dst = idx.Search(dst[:0], 0, ^uint64(0))
							idx.EstimateRange(0, ^uint64(0))
							idx.Metadata()
						}
					}(i)
				}
				for n := 0; n < 4; n++ {
					<-ready
				}
			}
			stopReaders := func() { close(stop); readers.Wait() }
			startReaders()
			defer func() {
				if stop != nil {
					stopReaders()
				}
			}()
			var source, logical, allocatedVolume uint64
			growth, reused := 0, 0
			lastFile := i.OnDiskBytes()
			plateau := uint64(0)
			// Address reuse tracking is test-only and exact (no probabilistic counter).
			commit := func(batch *Batch) {
				source++
				if err := i.ApplyWithGeneration(batch, source); err != nil {
					t.Fatal(err)
				}
				if i.SourceGeneration() != source {
					t.Fatal("source not committed")
				}
				logical += i.CommitStats().LogicalBytesWritten
				allocatedVolume += i.CommitStats().AllocatedBytes
				if i.CommitStats().RewrittenShards > 2*batch.Len()+4 {
					t.Fatal("whole-index rewrite")
				}
				bytes := i.OnDiskBytes()
				if bytes != lastFile {
					growth++
					lastFile = bytes
					checkGrowthState(t, i, entries, present)
					stopReaders()
					stop = nil
					if err := i.Close(); err != nil {
						t.Fatal(err)
					}
					i, err = OpenNamespace(dir, 1, cfg, DurabilitySync)
					if err != nil {
						t.Fatal(err)
					}
					checkGrowthState(t, i, entries, present)
					stop = make(chan struct{})
					startReaders()
				}
			}
			// Count reuse only when an address disappeared from both roots and
			// subsequently reappears; unchanged shared payloads are not reuse.
			oldUnion := slices.Clone(i.live)
			freed := make([]storage.Extent, 0)
			track := func() {
				for _, e := range oldUnion {
					if _, ok := slices.BinarySearchFunc(i.live, e.Ref, func(a storage.Extent, b pagealloc.Ref) int { return cmp.Compare(a.Ref, b) }); !ok {
						freed = append(freed, e)
					}
				}
				for _, e := range i.live {
					for n, f := range freed {
						if f.Ref == e.Ref {
							reused++
							freed = slices.Delete(freed, n, n+1)
							break
						}
					}
				}
				oldUnion = append(oldUnion[:0], i.live...)
			}
			batch := NewBatch(64)
			for cycle := 0; cycle < 160; cycle++ {
				base := cycle * 64
				if cycle >= 32 {
					base = 2048
				}
				batch.Reset()
				for n := base; n < base+64; n++ {
					present[n] = true
					batch.Append(Insert(entries[n].ImageID, entries[n].Timestamp))
				}
				commit(batch)
				track()
				active := min(2048, (cycle+1)*64)
				first := (cycle * 64) % active
				batch.Reset()
				for n := first; n < first+64; n++ {
					entries[n].Timestamp = uint64(cycle+1)*1000000 + uint64(n)
					batch.Append(Move(entries[n].ImageID, 0, entries[n].Timestamp))
				}
				commit(batch)
				track()
				batch.Reset()
				for n := first; n < first+64; n++ {
					present[n] = false
					batch.Append(Delete(entries[n].ImageID, 0))
				}
				commit(batch)
				track()
				batch.Reset()
				for n := first; n < first+64; n++ {
					present[n] = true
					batch.Append(Insert(entries[n].ImageID, entries[n].Timestamp))
				}
				commit(batch)
				track()
				if cycle >= 32 {
					batch.Reset()
					for n := 2048; n < 2112; n++ {
						present[n] = false
						batch.Append(Delete(entries[n].ImageID, 0))
					}
					commit(batch)
					track()
				}
				if cycle%16 == 15 || cycle == 159 {
					checkGrowthState(t, i, entries, present)
					live := extentCapacity(i.currentLive)
					retained := extentCapacity(i.live) - live
					payload := i.Len() * 16
					file := uint64(len(i.file.Bytes()))
					free := i.alloc.FreeBytes()
					if live+retained+free+1024 != file {
						t.Fatal("capacity accounting", live, retained, free, file)
					}
					t.Logf("cycle=%d file=%d payload=%d current-extents=%d retained-only=%d free=%d largest-free=%d growth=%d reused=%d shards=%d directory=%d logical=%d allocated-volume=%d", cycle+1, file, payload, live, retained, free, i.alloc.LargestFreeBytes(), growth, reused, len(i.view.Load().shards), len(i.view.Load().shards)*directoryEntrySize, logical, allocatedVolume)
				}
				if cycle == 63 {
					plateau = i.OnDiskBytes()
				}
				if cycle == 79 {
					stopReaders()
					stop = nil
					if err := i.Close(); err != nil {
						t.Fatal(err)
					}
					i, err = OpenNamespace(dir, 1, cfg, DurabilitySync)
					if err != nil {
						t.Fatal(err)
					}
					if i.SourceGeneration() != source {
						t.Fatal("reopen watermark")
					}
					stop = make(chan struct{})
					startReaders()
				}
			}
			stopReaders()
			stop = nil
			if growth < 3 || reused == 0 {
				t.Fatal("did not exercise growth/reuse", growth, reused)
			}
			if i.OnDiskBytes() != plateau {
				t.Fatal("bounded workload did not plateau", plateau, i.OnDiskBytes())
			}
			if err := i.Close(); err != nil {
				t.Fatal(err)
			}
			i, err = OpenNamespace(dir, 1, cfg, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			checkGrowthState(t, i, entries, present)
		})
	}
}
