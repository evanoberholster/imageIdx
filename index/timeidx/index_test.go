package timeidx

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testQuantum = uint64(1) << TimestampShift

func TestSearchExactFiltersLossyBoundaryMarkers(t *testing.T) {
	const offset = uint64(10_000)
	timestamps := map[ImageID]uint64{
		1: offset + testQuantum + 10,
		2: offset + testQuantum + 20,
		3: offset + testQuantum + 30,
		4: offset + 2*testQuantum + 1,
	}
	records := make([]Entry, 0, len(timestamps))
	for id, timestamp := range timestamps {
		records = append(records, Entry{Timestamp: timestamp, ImageID: id})
	}
	index := mustNewIndex(t, records, offset, Config{})

	candidates := index.Search(make([]ImageID, 0, 4), offset+testQuantum+15, offset+testQuantum+25)
	if len(candidates) != 3 {
		t.Fatalf("candidate count = %d, want 3", len(candidates))
	}
	exact := index.SearchExact(make([]ImageID, 0, 4), offset+testQuantum+15, offset+testQuantum+25,
		func(id ImageID) (uint64, bool) {
			timestamp, ok := timestamps[id]
			return timestamp, ok
		})
	if !equalIDSet(exact, []ImageID{2}) {
		t.Fatalf("exact result = %v, want [2]", exact)
	}
}

func TestAddDeleteAndRepeatedUpdateInActiveDelta(t *testing.T) {
	const offset = uint64(1_000)
	index := mustNewIndex(t, []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
		{Timestamp: offset + 2*testQuantum, ImageID: 2},
	}, offset, Config{DeltaCapacity: 16})

	batch := NewBatch(6)
	batch.Append(Delete(1, offset+testQuantum))
	batch.Append(Insert(1, offset+4*testQuantum))
	batch.Append(Delete(1, offset+4*testQuantum))
	batch.Append(Insert(1, offset+5*testQuantum))
	batch.Append(Delete(2, offset+2*testQuantum))
	batch.Append(Insert(3, offset+3*testQuantum))
	if err := index.Apply(batch); err != nil {
		t.Fatal(err)
	}

	all := index.Search(make([]ImageID, 0, 4), offset, offset+6*testQuantum)
	if !equalIDSet(all, []ImageID{1, 3}) {
		t.Fatalf("all = %v, want IDs 1 and 3", all)
	}
	old := index.Search(make([]ImageID, 0, 2), offset+testQuantum, offset+3*testQuantum)
	if len(old) != 0 {
		t.Fatalf("old ranges still contain %v", old)
	}
	latest := index.Search(make([]ImageID, 0, 2), offset+5*testQuantum, offset+6*testQuantum)
	if !equalIDSet(latest, []ImageID{1}) {
		t.Fatalf("latest = %v, want [1]", latest)
	}
}

func TestRotationAndCompaction(t *testing.T) {
	const offset = uint64(100)
	index := mustNewIndex(t, []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
		{Timestamp: offset + 2*testQuantum, ImageID: 2},
	}, offset, Config{DeltaCapacity: 4})

	batch := NewBatch(4)
	batch.Append(Delete(1, offset+testQuantum))
	batch.Append(Insert(1, offset+4*testQuantum))
	batch.Append(Insert(3, offset+3*testQuantum))
	batch.Append(Delete(2, offset+2*testQuantum))
	if err := index.Apply(batch); err != nil {
		t.Fatal(err)
	}

	batch.Reset()
	batch.Append(Insert(4, offset+5*testQuantum))
	if err := index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := index.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}

	all := index.Search(make([]ImageID, 0, 4), offset, offset+6*testQuantum)
	if !equalIDSet(all, []ImageID{1, 3, 4}) {
		t.Fatalf("result after compaction = %v", all)
	}
	view := index.directory.Load().entries[0].block.view.Load()
	if view.frozen != nil {
		t.Fatal("frozen delta remained after compaction")
	}
	if !equalMarkers(view.base.markers, []timemarker{3, 4}) || !equalIDs(view.base.imageIDs, []ImageID{3, 1}) {
		t.Fatalf("compacted base = (%v, %v)", view.base.markers, view.base.imageIDs)
	}
}

func TestBackpressureDoesNotCreateThirdDelta(t *testing.T) {
	index := mustNewIndex(t, nil, 0, Config{DeltaCapacity: 2})
	entry := &index.directory.Load().entries[0]
	view := entry.block.view.Load()
	view.active.appendAdd(1, 1)
	view.active.appendAdd(2, 2)
	entry.block.spare = nil
	entry.block.view.Store(&blockView{
		base:       view.base,
		frozen:     newDelta(2),
		active:     view.active,
		generation: view.generation + 1,
	})

	batch := NewBatch(1)
	batch.Append(Insert(3, 3*testQuantum))
	if err := index.Apply(batch); err != ErrBackpressure {
		t.Fatalf("error = %v, want %v", err, ErrBackpressure)
	}
	got := entry.block.view.Load()
	if got.frozen == nil || got.active != view.active {
		t.Fatal("backpressure changed the three-layer view")
	}
}

func TestBlockDirectoryDoesNotSplitEqualMarkers(t *testing.T) {
	const offset = uint64(100)
	records := []Entry{
		{Timestamp: offset + testQuantum + 1, ImageID: 1},
		{Timestamp: offset + testQuantum + 2, ImageID: 2},
		{Timestamp: offset + testQuantum + 3, ImageID: 3},
		{Timestamp: offset + 2*testQuantum, ImageID: 4},
	}
	index := mustNewIndex(t, records, offset, Config{BlockSize: 2})
	if index.Metadata().BlockCount != 2 {
		t.Fatalf("block count = %d, want 2", index.Metadata().BlockCount)
	}
	first := index.directory.Load().entries[0]
	if first.block.view.Load().base.len() != 3 {
		t.Fatalf("first block size = %d, want 3", first.block.view.Load().base.len())
	}
}

func TestConcurrentReadersUpdatesAndCompaction(t *testing.T) {
	const offset = uint64(1_000)
	records := make([]Entry, 256)
	for i := range records {
		records[i] = Entry{Timestamp: offset + uint64(i)*testQuantum, ImageID: ImageID(i + 1)}
	}
	index := mustNewIndex(t, records, offset, Config{BlockSize: 64, DeltaCapacity: 32})

	var stop atomic.Bool
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			buffer := make([]ImageID, 0, 512)
			for !stop.Load() {
				buffer = index.Search(buffer[:0], offset, offset+512*testQuantum)
			}
		}()
	}

	batch := NewBatch(1)
	currentMarker := make([]uint64, 128)
	for marker := range currentMarker {
		currentMarker[marker] = uint64(marker)
	}
	for iteration := 0; iteration < 400; iteration++ {
		position := iteration % 128
		id := ImageID(position + 1)
		oldMarker := currentMarker[position]
		newMarker := uint64(256 + position)
		batch.Reset()
		batch.Append(Move(id, offset+oldMarker*testQuantum, offset+newMarker*testQuantum))
		for {
			err := index.Apply(batch)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrBackpressure) {
				t.Fatal(err)
			}
			time.Sleep(time.Microsecond)
		}
		currentMarker[position] = newMarker
	}
	stop.Store(true)
	readers.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := index.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSteadyStateSearchAllocations(t *testing.T) {
	const offset = uint64(1_000)
	records := make([]Entry, 1_024)
	for i := range records {
		records[i] = Entry{Timestamp: offset + uint64(i)*testQuantum, ImageID: ImageID(i + 1)}
	}
	index := mustNewIndex(t, records, offset, Config{})
	buffer := make([]ImageID, 0, len(records))
	allocations := testing.AllocsPerRun(1_000, func() {
		buffer = index.Search(buffer[:0], offset, offset+uint64(len(records))*testQuantum)
	})
	if allocations != 0 {
		t.Fatalf("search allocations = %f, want 0", allocations)
	}
}

func TestSteadyStateUpdateAllocations(t *testing.T) {
	index := mustNewIndex(t, nil, 0, Config{DeltaCapacity: 4_096})
	batch := NewBatch(1)
	var sequence uint64
	allocations := testing.AllocsPerRun(1_000, func() {
		sequence++
		batch.Reset()
		batch.Append(Insert(1, sequence<<TimestampShift))
		if err := index.Apply(batch); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("update allocations = %f, want 0", allocations)
	}
}

func TestRejectsDuplicateImageID(t *testing.T) {
	_, err := New(Spec{Entries: []Entry{{Timestamp: 1, ImageID: 1}, {Timestamp: 2, ImageID: 1}}}, Config{})
	if err != ErrDuplicateImageID {
		t.Fatalf("error = %v, want %v", err, ErrDuplicateImageID)
	}
}

func mustNewIndex(t *testing.T, records []Entry, offset uint64, config Config) *Index {
	t.Helper()
	index, err := New(Spec{Offset: offset, Entries: records}, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Error(err)
		}
	})
	return index
}

func equalIDSet(got, want []ImageID) bool {
	got = slices.Clone(got)
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

// TestDoubleApplyCollapses proves the replay-idempotence property
// Checkpoint documents: applying the same batch twice (as open replay
// does for writes covered by both snapshot and WAL tail) reads
// identically, because per-ID delta state always resolves to the latest
// operation.
func TestDoubleApplyCollapses(t *testing.T) {
	const offset = uint64(1_000)
	index := mustNewIndex(t, []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
	}, offset, Config{BlockSize: 2, DeltaCapacity: 16})
	apply := func() {
		t.Helper()
		batch := NewBatch(3)
		batch.Append(Insert(2, offset+2*testQuantum))
		batch.Append(Move(1, offset+testQuantum, offset+3*testQuantum))
		if err := index.Apply(batch); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	apply()
	all := index.Search(make([]ImageID, 0, 4), offset, offset+4*testQuantum)
	if !equalIDSet(all, []ImageID{1, 2}) {
		t.Fatalf("doubled batch reads %v, want IDs 1 and 2 once each", all)
	}
	moved := index.Search(make([]ImageID, 0, 2), offset+3*testQuantum, offset+4*testQuantum)
	if !equalIDSet(moved, []ImageID{1}) {
		t.Fatalf("moved range reads %v, want [1]", moved)
	}
}
