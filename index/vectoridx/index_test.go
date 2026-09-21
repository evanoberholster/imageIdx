package vectoridx

import (
	"errors"
	"math/rand"
	"sync"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{Dimensions: 100, ElementType: ElementFloat32},
		{Dimensions: Dim512, ElementType: ElementInvalid},
		{Dimensions: Dim512, ElementType: ElementFloat32, M: 1},
		{Dimensions: Dim512, ElementType: ElementFloat32, EfConstruction: 5000},
		{Dimensions: 0, ElementType: ElementFloat32},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("cfg %+v: expected ErrInvalidConfig, got %v", cfg, err)
		}
	}
	idx, err := New(Config{SpaceID: NewSpaceID(IndexTypeImage, 1), Dimensions: Dim512, ElementType: ElementFloat32})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if got := idx.Config().M; got != DefaultM {
		t.Fatalf("default M = %d, want %d", got, DefaultM)
	}
	if err := idx.Verify(); err != nil {
		t.Fatalf("empty Verify: %v", err)
	}
}

func TestAddSearchSingleFloat(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(7))
	vec := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 42, vec)
	results, err := idx.SearchFloat32(vec, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != 42 || results[0].Score != 0 {
		t.Fatalf("single search = %+v, want [{42 0}]", results)
	}
	if got := idx.LiveCount(); got != 1 {
		t.Fatalf("LiveCount = %d, want 1", got)
	}
}

func TestAddDuplicateRejected(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(8))
	vec := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 1, vec)
	if err := idx.AddFloat32(1, vec); !errors.Is(err, ErrDuplicateNodeID) {
		t.Fatalf("duplicate live add = %v, want ErrDuplicateNodeID", err)
	}
	if err := idx.Delete(1); err != nil {
		t.Fatal(err)
	}
	// Re-adding a tombstoned ID replaces its vector; Delete+Add is an update.
	updated := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 1, updated)
	if _, err := idx.Compact(); err != nil {
		t.Fatal(err)
	}
	results, err := idx.SearchFloat32(updated, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != 1 || results[0].Score != 0 {
		t.Fatalf("re-add search = %+v", results)
	}
}

func TestDeleteSemantics(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Delete(999); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("delete unknown = %v, want ErrNodeNotFound", err)
	}
	rng := rand.New(rand.NewSource(9))
	vecs := make([][]float32, 10)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	if err := idx.Delete(3); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(3); err != nil {
		t.Fatalf("second delete (idempotent) = %v", err)
	}
	if got := idx.TombstoneCount(); got != 1 {
		t.Fatalf("TombstoneCount = %d, want 1", got)
	}
	// The tombstoned node must never be returned, for its own query included.
	for range 5 {
		results, err := idx.SearchFloat32(vecs[2], nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if containsResult(results, 3) {
			t.Fatalf("tombstoned node 3 returned: %+v", results)
		}
		if len(results) != 9 {
			t.Fatalf("expected 9 live results, got %d", len(results))
		}
	}
	// Graph stays traversable: every other node is still reachable by search.
	for i, v := range vecs {
		if i == 2 {
			continue
		}
		results, err := idx.SearchFloat32(v, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].ImageID != uint64(i+1) {
			t.Fatalf("node %d self-search = %+v", i+1, results)
		}
	}
}

func TestSparseNodeIDs(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(11))
	ids := []NodeID{1, 1 << 24, 77777777, 4000000000, 123456}
	vecs := make(map[NodeID][]float32, len(ids))
	for _, id := range ids {
		vec := randFloatVec(rng, Dim512)
		vecs[id] = vec
		mustAddFloat(t, idx, id, vec)
	}
	if got := idx.LiveCount(); got != len(ids) {
		t.Fatalf("LiveCount = %d, want %d", got, len(ids))
	}
	for id, vec := range vecs {
		results, err := idx.SearchFloat32(vec, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].ImageID != uint64(id) {
			t.Fatalf("sparse id %d self-search = %+v", id, results)
		}
	}
	if err := idx.Verify(); err != nil {
		t.Fatalf("Verify sparse: %v", err)
	}
}

func TestTypedAPIMismatchRejected(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.AddInt8(1, make([]int8, Dim512)); !errors.Is(err, ErrInvalidVector) {
		t.Fatalf("AddInt8 on float32 index = %v, want ErrInvalidVector", err)
	}
	if _, err := idx.SearchInt8(make([]int8, Dim512), nil, 1); !errors.Is(err, ErrInvalidVector) {
		t.Fatalf("SearchInt8 on float32 index = %v, want ErrInvalidVector", err)
	}
	if err := idx.AddFloat32(1, make([]float32, 100)); !errors.Is(err, ErrInvalidVector) {
		t.Fatalf("short vector add = %v, want ErrInvalidVector", err)
	}
	if _, err := idx.SearchFloat32(make([]float32, Dim512), nil, 0); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("k=0 search = %v, want ErrInvalidQuery", err)
	}
	results, err := idx.SearchFloat32(make([]float32, Dim512), nil, 5)
	if err != nil || len(results) != 0 {
		t.Fatalf("empty index search = %+v, %v; want empty, nil", results, err)
	}
}

func TestAllElementTypesRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	cfgs := []Config{
		{SpaceID: NewSpaceID(IndexTypeImage, 2), Dimensions: Dim512, ElementType: ElementInt8},
		{SpaceID: NewSpaceID(IndexTypeImage, 3), Dimensions: Dim512, ElementType: ElementUint8},
		{SpaceID: NewSpaceID(IndexTypeImage, 4), Dimensions: Dim512, ElementType: ElementInt16},
		{SpaceID: NewSpaceID(IndexTypeImage, 5), Dimensions: Dim512, ElementType: ElementUint16},
	}
	v8 := randInt8Vec(rng, Dim512)
	vu := make([]uint8, Dim512)
	for i := range vu {
		vu[i] = uint8(rng.Intn(256))
	}
	vi16 := make([]int16, Dim512)
	vu16 := make([]uint16, Dim512)
	for i := range vi16 {
		vi16[i] = int16(rng.Intn(65536) - 32768)
		vu16[i] = uint16(rng.Intn(65536))
	}
	idx8, _ := New(cfgs[0])
	defer idx8.Close()
	if err := idx8.AddInt8(5, v8); err != nil {
		t.Fatal(err)
	}
	res, err := idx8.SearchInt8(v8, nil, 1)
	if err != nil || len(res) != 1 || res[0].ImageID != 5 || res[0].Score != 0 {
		t.Fatalf("int8 round trip = %+v, %v", res, err)
	}
	idxU, _ := New(cfgs[1])
	defer idxU.Close()
	if err := idxU.AddUint8(6, vu); err != nil {
		t.Fatal(err)
	}
	res, err = idxU.SearchUint8(vu, nil, 1)
	if err != nil || len(res) != 1 || res[0].ImageID != 6 {
		t.Fatalf("uint8 round trip = %+v, %v", res, err)
	}
	idx16, _ := New(cfgs[2])
	defer idx16.Close()
	if err := idx16.AddInt16(7, vi16); err != nil {
		t.Fatal(err)
	}
	res, err = idx16.SearchInt16(vi16, nil, 1)
	if err != nil || len(res) != 1 || res[0].ImageID != 7 {
		t.Fatalf("int16 round trip = %+v, %v", res, err)
	}
	idxU16, _ := New(cfgs[3])
	defer idxU16.Close()
	if err := idxU16.AddUint16(8, vu16); err != nil {
		t.Fatal(err)
	}
	res, err = idxU16.SearchUint16(vu16, nil, 1)
	if err != nil || len(res) != 1 || res[0].ImageID != 8 {
		t.Fatalf("uint16 round trip = %+v, %v", res, err)
	}
}

func TestSearchIntoReusesCallerBuffer(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(13))
	var query []float32
	for i := NodeID(1); i <= 20; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	dst := make([]Result, 0, 32)
	dst, err = idx.SearchIntoFloat32(dst, query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(dst) != 5 {
		t.Fatalf("SearchInto len = %d, want 5", len(dst))
	}
	assertSortedByScore(t, dst)
	first := append([]Result(nil), dst...)
	// Second call reuses the same backing array without aliasing pooled memory:
	// mutating the first copy must not affect the next result.
	first[0].ImageID = 999999
	dst2, err := idx.SearchIntoFloat32(dst[:0], query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if dst2[0].ImageID == 999999 {
		t.Fatalf("pooled memory aliased caller buffer")
	}
	if len(dst2) != 5 || dst2[0].ImageID != 1 {
		t.Fatalf("second SearchInto = %+v", dst2)
	}
}

func TestConcurrentReadersSingleWriter(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(14))
	var query []float32
	for i := NodeID(1); i <= 50; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for r := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				_ = i
				_ = r
				if _, err := idx.SearchFloat32(query, nil, 5); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := NodeID(51); i <= 60; i++ {
			if err := idx.AddFloat32(i, randFloatVec(rng, Dim512)); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read/write: %v", err)
	}
	if err := idx.Verify(); err != nil {
		t.Fatalf("Verify after concurrency: %v", err)
	}
}

func TestClosedIndexErrors(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(1)), Dim512))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.AddFloat32(2, make([]float32, Dim512)); !errors.Is(err, ErrClosed) {
		t.Fatalf("add after close = %v", err)
	}
	if err := idx.Delete(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("delete after close = %v", err)
	}
	if _, err := idx.SearchFloat32(make([]float32, Dim512), nil, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("search after close = %v", err)
	}
}

func TestReAddAfterDeleteReplacesVector(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(41))
	old := randFloatVec(rng, Dim512)
	for i := NodeID(1); i <= 48; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 7 {
			vec = old
		}
		mustAddFloat(t, idx, i, vec)
	}
	if err := idx.AddFloat32(7, old); !errors.Is(err, ErrDuplicateNodeID) {
		t.Fatalf("duplicate live add = %v, want ErrDuplicateNodeID", err)
	}
	if err := idx.Delete(7); err != nil {
		t.Fatal(err)
	}
	updated := make([]float32, Dim512)
	for i := range updated {
		updated[i] = old[i] + 1
	}
	if err := idx.AddFloat32(7, updated); err != nil {
		t.Fatalf("re-add after delete: %v", err)
	}
	if got := idx.LiveCount(); got != 48 {
		t.Fatalf("LiveCount after re-add = %d, want 48", got)
	}
	if err := idx.Verify(); err != nil {
		t.Fatalf("Verify after re-add: %v", err)
	}
	results, err := idx.SearchFloat32(updated, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != 7 || results[0].Score != 0 {
		t.Fatalf("self-search after re-add = %+v", results)
	}
}

func TestReAddEntryAfterDelete(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(42))
	for i := NodeID(1); i <= 64; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	entry := idx.Metadata().Entry
	if err := idx.Delete(entry); err != nil {
		t.Fatal(err)
	}
	updated := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, entry, updated)
	if err := idx.Verify(); err != nil {
		t.Fatalf("Verify after entry re-add: %v", err)
	}
	results, err := idx.SearchFloat32(updated, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != ImageID(entry) || results[0].Score != 0 {
		t.Fatalf("entry self-search after re-add = %+v", results)
	}
}

func TestCompactResultAndBusy(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(71))
	for i := NodeID(1); i <= 30; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	for i := NodeID(1); i <= 10; i++ {
		if err := idx.Delete(i); err != nil {
			t.Fatal(err)
		}
	}
	idx.compactMu.Lock()
	if _, err := idx.Compact(); !errors.Is(err, ErrCompactionInProgress) {
		idx.compactMu.Unlock()
		t.Fatalf("concurrent Compact = %v, want ErrCompactionInProgress", err)
	}
	idx.compactMu.Unlock()
	res, err := idx.Compact()
	if err != nil {
		t.Fatal(err)
	}
	if res.NodesBefore != 30 || res.NodesAfter != 20 || res.TombstonesPurged != 10 {
		t.Fatalf("compaction counts = %+v", res)
	}
	want := int64(10) * int64(Dim512*4)
	if res.BytesReclaimed != want {
		t.Fatalf("BytesReclaimed = %d, want %d", res.BytesReclaimed, want)
	}
	if res.Duration <= 0 {
		t.Fatalf("Duration = %s, want > 0", res.Duration)
	}
	if got := idx.Stats().LastCompaction; got != res {
		t.Fatalf("Stats.LastCompaction = %+v, want %+v", got, res)
	}
	if got := idx.LiveCount(); got != 20 {
		t.Fatalf("LiveCount = %d, want 20", got)
	}
}
