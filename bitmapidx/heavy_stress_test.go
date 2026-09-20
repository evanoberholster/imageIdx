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
)

// TestHeavyStress is an opt-in, large-scale soak: many fields, a large item
// space, hundreds of readers, and multiple writers against one persistent
// store, with a maintainer checkpointing concurrently. It is gated behind
// BITMAPIDX_HEAVY so normal test runs stay fast. Scale with HEAVY_* env vars.
//
// Interpretation: HEAVY_ITEMS is the number of distinct database image IDs;
// HEAVY_FIELDS is the number of field indexes; each item is indexed into
// HEAVY_FIELDS_PER_ITEM of them. Total memberships is
// items*fieldsPerItem, which is what bounds load time and WAL.
func TestHeavyStress(t *testing.T) {
	if os.Getenv("BITMAPIDX_HEAVY") == "" {
		t.Skip("set BITMAPIDX_HEAVY=1 to run the heavy soak")
	}
	if testing.Short() {
		t.Skip("stress test")
	}
	fieldsN := heavyInt("HEAVY_FIELDS", 100)
	items := heavyInt("HEAVY_ITEMS", 10_000_000)
	perItem := heavyInt("HEAVY_FIELDS_PER_ITEM", 2)
	readers := heavyInt("HEAVY_READERS", 200)
	writers := heavyInt("HEAVY_WRITERS", 10)
	seconds := heavyInt("HEAVY_SECONDS", 30)
	valuesN := heavyInt("HEAVY_VALUES_PER_FIELD", 64)
	checkpointEvery := heavyInt("HEAVY_CHECKPOINT_EVERY", 2_000_000)
	if fieldsN < 1 || items < 1 || perItem < 1 || valuesN < 1 || writers < 0 || readers < 0 {
		t.Fatal("invalid heavy parameters")
	}
	if perItem > fieldsN {
		perItem = fieldsN
	}
	const firstType = IndexType(100)

	fields := make([]Field, fieldsN)
	for f := 0; f < fieldsN; f++ {
		fields[f] = enumField(firstType + IndexType(f))
	}
	config := FieldConfig{}
	dir := t.TempDir()
	store, err := NewPersistentFields(dir, fields, config, DurabilityOS)
	if err != nil {
		t.Fatalf("NewPersistentFields: %v", err)
	}
	defer store.Close()
	indexes := make([]*FieldIndex, fieldsN)
	for f := 0; f < fieldsN; f++ {
		index, ok := store.Lookup(firstType + IndexType(f))
		if !ok {
			t.Fatalf("field %d missing", f)
		}
		indexes[f] = index
	}

	values := make([][][]byte, fieldsN)
	for f := 0; f < fieldsN; f++ {
		values[f] = make([][]byte, valuesN)
		for v := 0; v < valuesN; v++ {
			values[f][v] = []byte(fmt.Sprintf("f%d-v%d", f, v))
		}
	}
	fieldFor := func(item, k int) int {
		return int((uint64(item)*2654435761 + uint64(k)*40503) % uint64(fieldsN))
	}
	valueFor := func(item int) int {
		return int((uint64(item) * 2246822519) % uint64(valuesN))
	}

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	loadStart := time.Now()

	batches := make([][]Entry, fieldsN)
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
		if err := indexes[f].Apply(FieldMutation{Add: entries}); err != nil {
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
			f := fieldFor(item, k)
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
		perWriter = fieldsN / writers
		if perWriter < 1 {
			perWriter = 1
		}
	}
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			src := rand.New(rand.NewSource(int64(w) + 1))
			lo := (w * perWriter) % fieldsN
			batch := make([]Entry, 0, 64)
			for {
				select {
				case <-stop:
					return
				default:
				}
				f := (lo + src.Intn(perWriter)) % fieldsN
				batch = batch[:0]
				for n := 0; n < 64; n++ {
					id := ImageID(src.Intn(items) + 1)
					v := src.Intn(valuesN)
					batch = append(batch, Entry{ImageID: id, Value: BytesValue(values[f][v])})
				}
				if src.Intn(4) == 0 {
					if err := indexes[f].Remove(batch...); err != nil {
						fail(err)
						return
					}
				} else {
					if err := indexes[f].Add(batch...); err != nil {
						fail(err)
						return
					}
				}
				writeOps.Add(64)
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
				f := src.Intn(fieldsN)
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

	t.Logf("HEAVY results: fields=%d items=%d memberships=%d readers=%d writers=%d values/field=%d",
		fieldsN, items, memberships, readers, writers, valuesN)
	t.Logf("load: %s (%.0f memberships/s), phase: %ds", loadDuration.Round(time.Millisecond),
		float64(memberships)/loadDuration.Seconds(), seconds)
	t.Logf("ops: reads=%d (%.0f/s) writes=%d (%.0f/s) checkpoints=%d",
		readOps.Load(), float64(readOps.Load())/float64(seconds),
		writeOps.Load(), float64(writeOps.Load())/float64(seconds), checks.Load())
	t.Logf("heap: start=%dMiB peak=%dMiB end=%dMiB goroutines=%d GOMAXPROCS=%d",
		memBefore.HeapAlloc>>20, peakHeap>>20, memAfter.HeapAlloc>>20, runtime.NumGoroutine(), runtime.GOMAXPROCS(0))

	reopened, err := OpenPersistentFields(dir, config, DurabilityOS)
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
		f := fieldFor(item, 0)
		v := valueFor(item)
		idx, _ := reopened.Lookup(firstType + IndexType(f))
		ok, err := idx.Contains(ImageID(item), BytesValue(values[f][v]))
		if err != nil || !ok {
			t.Fatalf("reopen spot-check item=%d field=%d ok=%v err=%v", item, f, ok, err)
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
