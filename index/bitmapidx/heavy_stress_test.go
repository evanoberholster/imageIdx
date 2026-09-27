package bitmapidx

import (
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
)

// TestHeavyStress is an opt-in, large-scale soak: many namespaces, a large item
// space, hundreds of readers, and multiple writers against one persistent
// store, with a maintainer checkpointing concurrently. It is gated behind
// BITMAPIDX_HEAVY so normal test runs stay fast. Scale with HEAVY_* env vars.
//
// Interpretation: HEAVY_ITEMS is the number of distinct database image IDs;
// HEAVY_NAMESPACES is the number of namespace indexes; each item is indexed into
// HEAVY_NAMESPACES_PER_ITEM of them. Total memberships is
// items*namespacesPerItem, which is what bounds load time and WAL.
func TestHeavyStress(t *testing.T) {
	if os.Getenv("BITMAPIDX_HEAVY") == "" {
		t.Skip("set BITMAPIDX_HEAVY=1 to run the heavy soak")
	}
	if testing.Short() {
		t.Skip("stress test")
	}
	namespacesN := heavyInt("HEAVY_NAMESPACES", 100)
	items := heavyInt("HEAVY_ITEMS", 10_000_000)
	perItem := heavyInt("HEAVY_NAMESPACES_PER_ITEM", 2)
	readers := heavyInt("HEAVY_READERS", 200)
	writers := heavyInt("HEAVY_WRITERS", 10)
	seconds := heavyInt("HEAVY_SECONDS", 30)
	valuesN := heavyInt("HEAVY_VALUES_PER_NAMESPACE", 64)
	checkpointEvery := heavyInt("HEAVY_CHECKPOINT_EVERY", 2_000_000)
	batchSize := heavyInt("HEAVY_BATCH", 64)
	durability := DurabilityOS
	if os.Getenv("HEAVY_DURABILITY") == "sync" {
		durability = DurabilitySync
	}
	if namespacesN < 1 || items < 1 || perItem < 1 || valuesN < 1 || writers < 0 || readers < 0 || batchSize < 1 {
		t.Fatal("invalid heavy parameters")
	}
	if perItem > namespacesN {
		perItem = namespacesN
	}
	const firstType = common.Namespace(100)

	namespaces := make([]NamespaceSpec, namespacesN)
	for f := 0; f < namespacesN; f++ {
		namespaces[f] = enumNamespace(firstType + common.Namespace(f))
	}
	config := NamespaceConfig{}
	dir := t.TempDir()
	// Bulk load is always written with OS durability; a sync-mode run reopens
	// the checkpointed store so the measured phase pays per-write fsync.
	loadDurability := durability
	if durability == DurabilitySync {
		loadDurability = DurabilityOS
	}
	store, err := NewIndex(dir, namespaces, config, loadDurability)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer func() { _ = store.Close() }()
	indexes := make([]*NamespaceIndex, namespacesN)
	for f := 0; f < namespacesN; f++ {
		index, ok := store.Lookup(firstType + common.Namespace(f))
		if !ok {
			t.Fatalf("namespace %d missing", f)
		}
		indexes[f] = index
	}

	values := make([][][]byte, namespacesN)
	for f := 0; f < namespacesN; f++ {
		values[f] = make([][]byte, valuesN)
		for v := 0; v < valuesN; v++ {
			values[f][v] = []byte(fmt.Sprintf("f%d-v%d", f, v))
		}
	}
	namespaceFor := func(item, k int) int {
		return int((uint64(item)*2654435761 + uint64(k)*40503) % uint64(namespacesN))
	}
	valueFor := func(item int) int {
		return int((uint64(item) * 2246822519) % uint64(valuesN))
	}

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	loadStart := time.Now()

	batches := make([][]Entry, namespacesN)
	for f := range batches {
		batches[f] = make([]Entry, 0, 4096)
	}
	memberships := 0
	sinceCheckpoint := 0
	flush := func(f int) error {
		if len(batches[f]) == 0 {
			return nil
		}
		entries := batches[f]
		batch := indexes[f].NewBatch()
		defer batch.Close()
		if err := batch.Add(entries...); err != nil {
			return err
		}
		if err := batch.Apply(); err != nil {
			return err
		}
		batches[f] = entries[:0]
		return nil
	}
	checkpoint := func(force bool) error {
		if !force && sinceCheckpoint < checkpointEvery {
			return nil
		}
		for f := range batches {
			if err := flush(f); err != nil {
				return err
			}
		}
		if err := store.Checkpoint(); err != nil {
			return err
		}
		sinceCheckpoint = 0
		return nil
	}
	reportEvery := items / 10
	if reportEvery == 0 {
		reportEvery = 1
	}
	for item := 1; item <= items; item++ {
		var chosen [16]int
		n := 0
		for k := 0; k < perItem; k++ {
			f := namespaceFor(item, k)
			duplicate := false
			for j := 0; j < n; j++ {
				if chosen[j] == f {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			chosen[n] = f
			n++
			v := valueFor(item)
			batches[f] = append(batches[f], Entry{ImageID: ImageID(item), Value: BytesValue(values[f][v])})
			if len(batches[f]) == cap(batches[f]) {
				if err := flush(f); err != nil {
					t.Fatalf("load flush: %v", err)
				}
				memberships += 4096
				sinceCheckpoint += 4096
				if err := checkpoint(false); err != nil {
					t.Fatalf("load checkpoint: %v", err)
				}
			}
		}
		if item%reportEvery == 0 {
			t.Logf("loaded %d/%d items (%.0f%%), memberships~%d, heap=%dMiB",
				item, items, 100*float64(item)/float64(items), memberships, memHeapMiB())
		}
	}
	for f := range batches {
		memberships += len(batches[f])
	}
	if err := checkpoint(true); err != nil {
		t.Fatalf("final load checkpoint: %v", err)
	}
	loadDuration := time.Since(loadStart)
	if durability != loadDurability {
		if err := store.Close(); err != nil {
			t.Fatalf("close after load: %v", err)
		}
		store, err = OpenIndex(dir, config, durability)
		if err != nil {
			t.Fatalf("reopen for sync durability: %v", err)
		}
		indexes = indexes[:0]
		for f := 0; f < namespacesN; f++ {
			index, ok := store.Lookup(firstType + common.Namespace(f))
			if !ok {
				t.Fatalf("namespace %d missing after reopen", f)
			}
			indexes = append(indexes, index)
		}
	}

	stop := make(chan struct{})
	var (
		readOps  atomic.Int64
		writeOps atomic.Int64
		checks   atomic.Int64
		errOnce  sync.Once
		loadErr  error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() { loadErr = err })
	}
	var workers sync.WaitGroup

	var writersWG sync.WaitGroup
	perWriter := 1
	if writers > 0 {
		perWriter = namespacesN / writers
		if perWriter < 1 {
			perWriter = 1
		}
	}
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			src := rand.New(rand.NewSource(int64(w) + 1))
			lo := (w * perWriter) % namespacesN
			entries := make([]Entry, 0, batchSize)
			for {
				select {
				case <-stop:
					return
				default:
				}
				f := (lo + src.Intn(perWriter)) % namespacesN
				entries = entries[:0]
				for n := 0; n < batchSize; n++ {
					id := ImageID(src.Intn(items) + 1)
					v := src.Intn(valuesN)
					entries = append(entries, Entry{ImageID: id, Value: BytesValue(values[f][v])})
				}
				wb := indexes[f].NewBatch()
				var err error
				if src.Intn(4) == 0 {
					err = wb.Remove(entries...)
				} else {
					err = wb.Add(entries...)
				}
				if err == nil {
					err = wb.Apply()
				}
				wb.Close()
				if err != nil {
					fail(err)
					return
				}
				writeOps.Add(int64(batchSize))
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		workers.Add(1)
		go func(r int) {
			defer workers.Done()
			src := rand.New(rand.NewSource(int64(r) + 1_000_003))
			for {
				select {
				case <-stop:
					return
				default:
				}
				f := src.Intn(namespacesN)
				v := src.Intn(valuesN)
				value := BytesValue(values[f][v])
				if _, err := indexes[f].CountValues(value); err != nil {
					fail(err)
					return
				}
				if _, err := indexes[f].MatchValues(value); err != nil {
					fail(err)
					return
				}
				id := ImageID(src.Intn(items) + 1)
				if _, err := indexes[f].Contains(id, value); err != nil {
					fail(err)
					return
				}
				readOps.Add(3)
			}
		}(r)
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := store.Checkpoint(); err != nil {
					fail(err)
					return
				}
				checks.Add(1)
			}
		}
	}()

	deadline := time.After(time.Duration(seconds) * time.Second)
	var peakHeap uint64
	doneStats := make(chan struct{})
	go func() {
		defer close(doneStats)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peakHeap {
					peakHeap = m.HeapAlloc
				}
			}
		}
	}()
	<-deadline
	close(stop)
	writersWG.Wait()
	workers.Wait()
	<-doneStats

	if loadErr != nil {
		t.Fatalf("concurrent phase error: %v", loadErr)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatalf("final checkpoint: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&memAfter)

	t.Logf("HEAVY results: namespaces=%d items=%d memberships=%d readers=%d writers=%d values/namespace=%d",
		namespacesN, items, memberships, readers, writers, valuesN)
	t.Logf("load: %s (%.0f memberships/s), phase: %ds", loadDuration.Round(time.Millisecond),
		float64(memberships)/loadDuration.Seconds(), seconds)
	t.Logf("ops: reads=%d (%.0f/s) writes=%d (%.0f/s) checkpoints=%d",
		readOps.Load(), float64(readOps.Load())/float64(seconds),
		writeOps.Load(), float64(writeOps.Load())/float64(seconds), checks.Load())
	t.Logf("heap: start=%dMiB peak=%dMiB end=%dMiB goroutines=%d GOMAXPROCS=%d",
		memBefore.HeapAlloc>>20, peakHeap>>20, memAfter.HeapAlloc>>20, runtime.NumGoroutine(), runtime.GOMAXPROCS(0))

	reopened, err := OpenIndex(dir, config, DurabilityOS)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	sample := items / 1000
	if sample < 1 {
		sample = 1
	}
	for s := 0; s < 1000; s++ {
		item := (s*sample)%items + 1
		f := namespaceFor(item, 0)
		v := valueFor(item)
		idx, _ := reopened.Lookup(firstType + common.Namespace(f))
		ok, err := idx.Contains(ImageID(item), BytesValue(values[f][v]))
		if err != nil || !ok {
			t.Fatalf("reopen spot-check item=%d namespace=%d ok=%v err=%v", item, f, ok, err)
		}
	}
}

func memHeapMiB() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc >> 20
}

// heavyInt is stressInt but allows an explicit zero, so writers=0 can be used
// as a diagnostics baseline.
func heavyInt(name string, def int) int {
	if raw := os.Getenv(name); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return n
		}
	}
	return def
}
