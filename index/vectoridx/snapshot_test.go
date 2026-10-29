package vectoridx

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/rand"
	"slices"
	"sync"
	"testing"

	"github.com/evanoberholster/imageIdx/roar"
)

func TestSnapshotPinsView(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(301))
	vecs := make([][]float32, 20)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	snap := idx.Snapshot()
	if snap == nil {
		t.Fatal("nil snapshot")
	}
	defer snap.Release()
	if got := snap.LiveCount(); got != 20 {
		t.Fatalf("snapshot LiveCount = %d, want 20", got)
	}
	if err := idx.Delete(1); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(2); err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 21, randFloatVec(rng, Dim512))
	if got := idx.LiveCount(); got != 19 {
		t.Fatalf("index LiveCount = %d, want 19", got)
	}
	if got := snap.LiveCount(); got != 20 {
		t.Fatalf("snapshot LiveCount after writes = %d, want 20", got)
	}
	results, err := snap.SearchFloat32(vecs[0], nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 1 {
		t.Fatalf("snapshot search = %+v, want node 1", results)
	}
	live, err := idx.SearchFloat32(vecs[0], nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) == 1 && live[0].ValueID == 1 {
		t.Fatalf("live search returned deleted node 1: %+v", live)
	}
}

func TestChunkedVectorCopyOnWrite(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(307))
	for i := NodeID(1); i <= 150; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
	oldSnap := idx.Snapshot()
	defer oldSnap.Release()
	oldView := oldSnap.view
	if got, want := oldView.vectorChunkCount(), (150+vectorChunkVectors-1)>>vectorChunkShift; got != want {
		t.Fatalf("chunks = %d, want %d", got, want)
	}
	const id = NodeID(70)
	oldOrd := int(id - 1)
	oldChunk := oldView.vectors[oldOrd>>vectorChunkShift]
	oldBytes := append([]byte(nil), oldView.vectorAt(oldOrd)...)
	newVec := randFloatVec(rng, Dim512)
	if err := idx.Delete(id); err != nil {
		t.Fatal(err)
	}
	if err := idx.AddFloat32(id, newVec); err != nil {
		t.Fatal(err)
	}
	current := idx.view.Load()
	newChunk := current.vectors[oldOrd>>vectorChunkShift]
	if &newChunk[0] == &oldChunk[0] {
		t.Fatal("resurrect did not copy the vector chunk")
	}
	if !bytes.Equal(oldView.vectorAt(oldOrd), oldBytes) {
		t.Fatal("published snapshot vector changed after resurrect")
	}
	if !bytes.Equal(current.vectorAt(oldOrd), encodeRawFloat32(nil, newVec)) {
		t.Fatal("new snapshot did not receive resurrected vector")
	}
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotResurrectRace(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(308))
	vecs := make([][]float32, 64)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	old := idx.Snapshot()
	defer old.Release()
	query := vecs[0]
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := old.ExactSearchFloat32(query, nil, 5); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 20; i++ {
			id := NodeID(i)
			if err := idx.Delete(id); err != nil {
				t.Error(err)
				return
			}
			if err := idx.AddFloat32(id, randFloatVec(rng, Dim512)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	if err := idx.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAfterClose(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	snap := idx.Snapshot()
	defer snap.Release()
	if _, err := snap.SearchFloat32(make([]float32, Dim512), nil, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed snapshot search = %v, want ErrClosed", err)
	}
}

func TestSearchOptionsCancellation(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(302))
	for i := NodeID(1); i <= 32; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	query := randFloatVec(rng, Dim512)
	if _, err := idx.SearchFloat32WithOptions(ctx, query, SearchOptions{K: 5, FilterHint: UnknownFilterHint}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SearchFloat32WithOptions = %v, want context.Canceled", err)
	}
	if _, err := idx.SearchCandidatesFloat32(ctx, []NodeID{1, 2, 3}, query, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("SearchCandidatesFloat32 = %v, want context.Canceled", err)
	}
	if _, err := idx.ExactSearchFloat32Context(ctx, query, nil, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExactSearchFloat32Context = %v, want context.Canceled", err)
	}
}

func TestSearchCandidatesMatchesExact(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(303))
	vecs := make([][]float32, 40)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	query := vecs[3]
	candidates := []NodeID{1, 4, 7, 12, 18, 25, 31, 40}
	require := func(id NodeID) bool { return slices.Contains(candidates, id) }
	want, err := idx.ExactSearchFloat32(query, require, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, err := idx.SearchCandidatesFloat32(context.Background(), candidates, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate results = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ValueID != want[i].ValueID || got[i].Score != want[i].Score {
			t.Fatalf("candidate result %d = %+v, want %+v", i, got[i], want[i])
		}
		if math.Abs(float64(got[i].Distance)-float64(want[i].Distance)) > 1e-4 {
			t.Fatalf("candidate distance %d = %v, want %v", i, got[i].Distance, want[i].Distance)
		}
	}
	if err := idx.Delete(7); err != nil {
		t.Fatal(err)
	}
	want, err = idx.ExactSearchFloat32(query, require, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, err = idx.SearchCandidatesFloat32(context.Background(), candidates, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate results after delete = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ValueID != want[i].ValueID {
			t.Fatalf("candidate result after delete %d = %d, want %d", i, got[i].ValueID, want[i].ValueID)
		}
	}
}

func TestSearchResultDistance(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(304))
	vecs := make([][]float32, 8)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	query := vecs[2]
	results, err := idx.SearchFloat32(query, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		stored := vecs[int(r.ValueID)-1]
		want := SquaredL2Float32(query, stored)
		if math.Abs(float64(r.Distance)-want) > 1e-4 {
			t.Fatalf("Distance for %d = %v, want %v", r.ValueID, r.Distance, want)
		}
	}
}

func TestSearchOptionsSteadyStateAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are calibrated for non-race builds")
	}
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(306))
	for i := NodeID(1); i <= 200; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	query := randFloatVec(rng, Dim512)
	opts := SearchOptions{K: 10, FilterHint: UnknownFilterHint}
	dst := make([]Result, 0, 10)
	for range 20 {
		dst, err = idx.SearchIntoFloat32WithOptions(context.Background(), dst[:0], query, opts)
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := testing.AllocsPerRun(50, func() {
		dst, err = idx.SearchIntoFloat32WithOptions(context.Background(), dst[:0], query, opts)
		if err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("SearchIntoFloat32WithOptions allocs = %v, want 0", n)
	}
	snap := idx.Snapshot()
	defer snap.Release()
	dst = dst[:0]
	for range 20 {
		dst, err = snap.SearchIntoFloat32WithOptions(context.Background(), dst[:0], query, opts)
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := testing.AllocsPerRun(50, func() {
		dst, err = snap.SearchIntoFloat32WithOptions(context.Background(), dst[:0], query, opts)
		if err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("snapshot SearchInto allocs = %v, want 0", n)
	}
	candidates := make([]NodeID, 20)
	for i := range candidates {
		candidates[i] = NodeID(i + 1)
	}
	dst = dst[:0]
	for range 20 {
		dst, err = idx.SearchCandidatesIntoFloat32(context.Background(), dst[:0], candidates, query, 10)
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := testing.AllocsPerRun(50, func() {
		dst, err = idx.SearchCandidatesIntoFloat32(context.Background(), dst[:0], candidates, query, 10)
		if err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("SearchCandidatesIntoFloat32 allocs = %v, want 0", n)
	}
}

func TestBitmapFilter(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(305))
	for i := NodeID(1); i <= 12; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	bitmap, err := roar.FromSorted([]uint64{2, 5, 9})
	if err != nil {
		t.Fatal(err)
	}
	results, err := idx.SearchFloat32(randFloatVec(rng, Dim512), BitmapFilter(bitmap), 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.ValueID != 2 && r.ValueID != 5 && r.ValueID != 9 {
			t.Fatalf("bitmap filter leaked result %d", r.ValueID)
		}
	}
}
