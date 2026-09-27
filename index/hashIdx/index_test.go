package hashidx

import (
	"math/rand"
	"testing"

	"github.com/evanoberholster/imageIdx/roar"
)

func mustNewIndex(t *testing.T, cfg Config) *Index {
	t.Helper()
	idx, err := newIndex(t.TempDir(), testPerceptualSpec(cfg), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func pdqFrom(seed uint64) PDQHash {
	var hash PDQHash
	state := seed + 0x9E3779B97F4A7C15
	for word := 0; word < 4; word++ {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		hash[word] = z ^ (z >> 31)
	}
	return hash
}

func entryFromSeed(seed uint64) Entry {
	return Entry{
		ImageID: ImageID(seed + 1),
		PHash64: pdqFrom(seed)[0],
		PDQ:     pdqFrom(seed ^ 0xAAAAAAAAAAAAAAAA),
	}
}

type idSet map[ImageID]struct{}

func (s idSet) Contains(id ImageID) bool {
	_, ok := s[id]
	return ok
}

func TestNewEmptyIndex(t *testing.T) {
	idx := mustNewIndex(t, Config{})
	metadata := idx.Metadata()
	if metadata.Generation != 1 || metadata.RecordCount != 0 || metadata.PageCount != 0 {
		t.Fatalf("metadata = %+v", metadata)
	}
	if metadata.RecordsPerPage != DefaultRecordsPerPage {
		t.Fatalf("records per page = %d", metadata.RecordsPerPage)
	}
	if got := idx.SearchPHashRadius(nil, 0, 64, nil); len(got) != 0 {
		t.Fatalf("phash radius on empty = %v", got)
	}
	if got := idx.SearchPDQRadius(nil, PDQHash{}, 256, nil); len(got) != 0 {
		t.Fatalf("pdq radius on empty = %v", got)
	}
	if got := idx.SearchPHashTopK(nil, 0, 10, nil, nil); len(got) != 0 {
		t.Fatalf("phash topk on empty = %v", got)
	}
	if _, ok := idx.EntryAt(0); ok {
		t.Fatal("entry at ordinal 0 on empty index")
	}
}

func TestApplyKeepsPlanesSynchronized(t *testing.T) {
	idx := mustNewIndex(t, Config{RecordsPerPage: 16})
	const total = 40
	entries := make([]Entry, total)
	for k := range entries {
		entries[k] = entryFromSeed(uint64(k))
	}
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	metadata := idx.Metadata()
	if metadata.RecordCount != total {
		t.Fatalf("record count = %d, want %d", metadata.RecordCount, total)
	}
	// 40 records with 16 per page: two sealed pages and one 8-record active.
	if metadata.PageCount != 3 {
		t.Fatalf("page count = %d, want 3", metadata.PageCount)
	}
	view := idx.view.Load()
	if len(view.pages) != 2 || view.activeCount != 8 {
		t.Fatalf("layout: pages=%d active=%d", len(view.pages), view.activeCount)
	}
	for k, want := range entries {
		got, ok := idx.EntryAt(Ordinal(k))
		if !ok {
			t.Fatalf("ordinal %d missing", k)
		}
		if got != want {
			t.Fatalf("ordinal %d = %+v, want %+v", k, got, want)
		}
	}
	if _, ok := idx.EntryAt(Ordinal(total)); ok {
		t.Fatal("ordinal past end should be absent")
	}
}

func TestSealOnExactBoundary(t *testing.T) {
	idx := mustNewIndex(t, Config{RecordsPerPage: 16})
	entries := make([]Entry, 16)
	for k := range entries {
		entries[k] = entryFromSeed(uint64(k))
	}
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	view := idx.view.Load()
	if len(view.pages) != 1 || view.active != nil || view.activeCount != 0 {
		t.Fatalf("after exact fill: pages=%d active=%v count=%d", len(view.pages), view.active, view.activeCount)
	}
	if idx.Metadata().PageCount != 1 {
		t.Fatalf("page count = %d, want 1", idx.Metadata().PageCount)
	}
	if err := idx.Add(entryFromSeed(999)); err != nil {
		t.Fatal(err)
	}
	if idx.Metadata().PageCount != 2 || idx.Metadata().RecordCount != 17 {
		t.Fatalf("after one more: %+v", idx.Metadata())
	}
}

func TestApplyValidationAndClosed(t *testing.T) {
	idx := mustNewIndex(t, Config{})
	if err := idx.Add(Entry{ImageID: 0}); err != ErrInvalidEntry {
		t.Fatalf("zero image ID error = %v", err)
	}
	bad := []Entry{entryFromSeed(1), {ImageID: 0}}
	if err := idx.Apply(bad); err != ErrInvalidEntry {
		t.Fatalf("batch with zero image ID error = %v", err)
	}
	if idx.Metadata().RecordCount != 0 {
		t.Fatal("invalid batch published records")
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add(entryFromSeed(1)); err != ErrClosed {
		t.Fatalf("apply after close = %v", err)
	}
	if got := idx.SearchPHashRadius(nil, 0, 64, nil); len(got) != 0 {
		t.Fatalf("search after close = %v", got)
	}
}

func TestPHashRadius(t *testing.T) {
	idx := mustNewIndex(t, Config{RecordsPerPage: 8})
	hashes := []uint64{
		0x0,         // distance 0 from 0
		0x1,         // 1
		0x3,         // 2
		0x7,         // 3
		^uint64(0),  // 64
		1 << 62,     // 1
		0x1 | 1<<40, // 2
	}
	for k, hash := range hashes {
		if err := idx.Add(Entry{ImageID: ImageID(k + 1), PHash64: hash}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		maxDistance uint8
		wantIDs     []ImageID
		wantDist    []uint16
	}{
		{0, []ImageID{1}, []uint16{0}},
		{1, []ImageID{1, 2, 6}, []uint16{0, 1, 1}},
		{2, []ImageID{1, 2, 3, 6, 7}, []uint16{0, 1, 2, 1, 2}},
		{3, []ImageID{1, 2, 3, 4, 6, 7}, []uint16{0, 1, 2, 3, 1, 2}},
		{64, []ImageID{1, 2, 3, 4, 5, 6, 7}, []uint16{0, 1, 2, 3, 64, 1, 2}},
	} {
		got := idx.SearchPHashRadius(nil, 0, tc.maxDistance, nil)
		if len(got) != len(tc.wantIDs) {
			t.Fatalf("radius %d result count = %d, want %d: %v", tc.maxDistance, len(got), len(tc.wantIDs), got)
		}
		for k := range got {
			if got[k].ImageID != tc.wantIDs[k] || got[k].Distance != tc.wantDist[k] {
				t.Fatalf("radius %d result[%d] = %+v, want id %d distance %d",
					tc.maxDistance, k, got[k], tc.wantIDs[k], tc.wantDist[k])
			}
		}
	}
	// Missing matches past the max.
	if got := idx.SearchPHashRadius(nil, 0, 0, nil); got[0].Distance != 0 {
		t.Fatalf("distance = %d", got[0].Distance)
	}
}

func TestPDQRadiusAgainstNaive(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const total = 500
	entries := make([]Entry, total)
	for k := range entries {
		entries[k] = Entry{
			ImageID: ImageID(k + 1),
			PHash64: rng.Uint64(),
			PDQ:     pdqFrom(uint64(k) * 31),
		}
	}
	idx := mustNewIndex(t, Config{RecordsPerPage: 64})
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	query := pdqFrom(123456)
	for _, maxDistance := range []uint16{0, 1, 8, 33, 128, 256} {
		want := make([]Result, 0, total)
		for _, entry := range entries {
			distance := pdqDistance(query, entry.PDQ)
			if distance <= maxDistance {
				want = append(want, Result{ImageID: entry.ImageID, Distance: distance})
			}
		}
		got := idx.SearchPDQRadius(nil, query, maxDistance, nil)
		assertResultsEqual(t, got, want, maxDistance)
	}
}

func TestPDQRadiusCandidateFilter(t *testing.T) {
	const total = 300
	entries := make([]Entry, total)
	for k := range entries {
		entries[k] = Entry{ImageID: ImageID(k + 1), PDQ: pdqFrom(uint64(k) * 7)}
	}
	idx := mustNewIndex(t, Config{RecordsPerPage: 32})
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	query := pdqFrom(99)
	candidate := idSet{}
	for _, entry := range entries {
		if entry.ImageID%3 == 0 {
			candidate[entry.ImageID] = struct{}{}
		}
	}
	want := make([]Result, 0, total)
	for _, entry := range entries {
		if _, ok := candidate[entry.ImageID]; !ok {
			continue
		}
		distance := pdqDistance(query, entry.PDQ)
		if distance <= 64 {
			want = append(want, Result{ImageID: entry.ImageID, Distance: distance})
		}
	}
	got := idx.SearchPDQRadius(nil, query, 64, candidate)
	assertResultsEqual(t, got, want, 64)

	empty := idSet{}
	if got := idx.SearchPDQRadius(nil, query, 256, empty); len(got) != 0 {
		t.Fatalf("empty candidate set returned %v", got)
	}
}

func TestRoarBitmapAsCandidateSet(t *testing.T) {
	idx := mustNewIndex(t, Config{})
	for k := 0; k < 20; k++ {
		if err := idx.Add(Entry{ImageID: ImageID(k + 1), PDQ: pdqFrom(uint64(k))}); err != nil {
			t.Fatal(err)
		}
	}
	bitmap := roar.FromUnsorted([]uint64{2, 4, 6})
	got := idx.SearchPDQRadius(nil, pdqFrom(3), 256, bitmap)
	want := []ImageID{2, 4, 6}
	if len(got) != len(want) {
		t.Fatalf("bitmap candidates = %v", got)
	}
	for k := range want {
		if got[k].ImageID != want[k] {
			t.Fatalf("bitmap candidates[%d] = %d, want %d", k, got[k].ImageID, want[k])
		}
	}
}

func TestTopKAgainstNaive(t *testing.T) {
	const total = 400
	entries := make([]Entry, total)
	for k := range entries {
		entries[k] = Entry{ImageID: ImageID(k + 1), PDQ: pdqFrom(uint64(k) * 13)}
	}
	idx := mustNewIndex(t, Config{RecordsPerPage: 64})
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	query := pdqFrom(5150)
	want := make([]Result, 0, total)
	for _, entry := range entries {
		want = append(want, Result{ImageID: entry.ImageID, Distance: pdqDistance(query, entry.PDQ)})
	}
	slicesSortResults(want)
	for _, k := range []int{1, 10, 100, total + 10} {
		limit := min(k, len(want))
		got := idx.SearchPDQTopK(nil, query, k, nil, nil)
		assertResultsEqual(t, got, want[:limit], 0)
	}
	if got := idx.SearchPDQTopK(nil, query, 0, nil, nil); len(got) != 0 {
		t.Fatalf("k=0 returned %v", got)
	}
}

func TestTopKDeterministicTies(t *testing.T) {
	idx := mustNewIndex(t, Config{})
	query := pdqFrom(1)
	for k := 0; k < 6; k++ {
		if err := idx.Add(Entry{ImageID: ImageID(k + 1), PDQ: query}); err != nil {
			t.Fatal(err)
		}
	}
	got := idx.SearchPDQTopK(nil, query, 3, nil, nil)
	want := []Result{
		{ImageID: 1, Distance: 0},
		{ImageID: 2, Distance: 0},
		{ImageID: 3, Distance: 0},
	}
	assertResultsEqual(t, got, want, 0)
}

func TestPHashTopK(t *testing.T) {
	idx := mustNewIndex(t, Config{})
	hashes := []uint64{0xF0F0F0F0F0F0F0F0, 0, ^uint64(0), 0x0F0F0F0F0F0F0F0F}
	for k, hash := range hashes {
		if err := idx.Add(Entry{ImageID: ImageID(k + 1), PHash64: hash}); err != nil {
			t.Fatal(err)
		}
	}
	got := idx.SearchPHashTopK(nil, 0, 2, nil, &Workspace{})
	if len(got) != 2 || got[0].ImageID != 2 || got[0].Distance != 0 {
		t.Fatalf("phash topk = %v", got)
	}
	if got[1].Distance > 32 {
		t.Fatalf("phash topk second distance = %d", got[1].Distance)
	}
}

func TestSearchIndependentHashes(t *testing.T) {
	idx := mustNewIndex(t, Config{})
	// Same pHash, different PDQ, and vice versa.
	entries := []Entry{
		{ImageID: 1, PHash64: 42, PDQ: pdqFrom(1)},
		{ImageID: 2, PHash64: 42, PDQ: pdqFrom(2)},
		{ImageID: 3, PHash64: 999, PDQ: pdqFrom(1)},
	}
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	near := idx.SearchPHashRadius(nil, 42, 0, nil)
	if len(near) != 2 || near[0].ImageID != 1 || near[1].ImageID != 2 {
		t.Fatalf("phash-only near = %v", near)
	}
	pdqNear := idx.SearchPDQRadius(nil, pdqFrom(1), 0, nil)
	if len(pdqNear) != 2 || pdqNear[0].ImageID != 1 || pdqNear[1].ImageID != 3 {
		t.Fatalf("pdq-only near = %v", pdqNear)
	}
}

func TestPublishedViewDoesNotSeeLaterAppends(t *testing.T) {
	idx := mustNewIndex(t, Config{RecordsPerPage: 64})
	first := entryFromSeed(1)
	if err := idx.Apply([]Entry{first}); err != nil {
		t.Fatal(err)
	}
	token := idx.readers.Acquire()
	old := idx.view.Load()
	second := entryFromSeed(2)
	if err := idx.Apply([]Entry{second}); err != nil {
		idx.readers.Release(token)
		t.Fatal(err)
	}
	if old.count != 1 {
		idx.readers.Release(token)
		t.Fatalf("old view count changed to %d", old.count)
	}
	if got := viewSearchPHashRadius(old, second.PHash64, 0, nil); len(got) != 0 {
		idx.readers.Release(token)
		t.Fatalf("old view observed a later append: %v", got)
	}
	fresh := idx.view.Load()
	if got := viewSearchPHashRadius(fresh, second.PHash64, 0, nil); len(got) != 1 {
		idx.readers.Release(token)
		t.Fatalf("new view missing published append: %v", got)
	}
	idx.readers.Release(token)
}

// viewSearchPHashRadius is a helper for tests that need to query a specific
// view directly.
func viewSearchPHashRadius(view *indexView, query uint64, maxDistance uint8, candidates CandidateSet) []Result {
	return searchPHashRadius(nil, view, query, maxDistance, candidates)
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	idx := mustNewIndex(t, Config{RecordsPerPage: 128})
	seed := make([]Entry, 256)
	for k := range seed {
		seed[k] = entryFromSeed(uint64(k))
	}
	if err := idx.Apply(seed); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{}, 4)
	for reader := 0; reader < 4; reader++ {
		go func(seed int64) {
			defer func() { done <- struct{}{} }()
			rng := rand.New(rand.NewSource(seed))
			dst := make([]Result, 0, 64)
			workspace := &Workspace{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				query := pdqFrom(uint64(rng.Intn(512)))
				dst = idx.SearchPDQRadius(dst[:0], query, 32, nil)
				_ = idx.SearchPDQTopK(dst[:0], query, 8, nil, workspace)
			}
		}(int64(reader))
	}
	for round := 0; round < 50; round++ {
		entries := make([]Entry, 32)
		for k := range entries {
			entries[k] = entryFromSeed(uint64(10_000 + round*32 + k))
		}
		if err := idx.Apply(entries); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	for reader := 0; reader < 4; reader++ {
		<-done
	}
	if idx.Metadata().RecordCount != 256+50*32 {
		t.Fatalf("record count = %d", idx.Metadata().RecordCount)
	}
}

func TestSearchZeroAllocations(t *testing.T) {
	const total = 1024
	entries := make([]Entry, total)
	for k := range entries {
		entries[k] = entryFromSeed(uint64(k))
	}
	idx := mustNewIndex(t, Config{})
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	queryHash := uint64(0xDEADBEEF)
	queryPDQ := pdqFrom(78)
	dst := make([]Result, 0, total)
	workspace := &Workspace{}
	// Warm the heap scratch before measuring.
	_ = idx.SearchPDQTopK(dst[:0], queryPDQ, 16, nil, workspace)

	if allocs := testing.AllocsPerRun(1000, func() {
		dst = idx.SearchPHashRadius(dst[:0], queryHash, 64, nil)
	}); allocs != 0 {
		t.Fatalf("phash radius allocs = %f", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		dst = idx.SearchPDQRadius(dst[:0], queryPDQ, 256, nil)
	}); allocs != 0 {
		t.Fatalf("pdq radius allocs = %f", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		dst = idx.SearchPHashTopK(dst[:0], queryHash, 16, nil, workspace)
	}); allocs != 0 {
		t.Fatalf("phash topk allocs = %f", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		dst = idx.SearchPDQTopK(dst[:0], queryPDQ, 16, nil, workspace)
	}); allocs != 0 {
		t.Fatalf("pdq topk allocs = %f", allocs)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, recordsPerPage := range []uint32{1, 7, 9, maxRecordsPerPage + 8} {
		if _, err := newIndex(t.TempDir(), testPerceptualSpec(Config{RecordsPerPage: recordsPerPage}), DurabilityOS); err != ErrInvalidConfig {
			t.Fatalf("records per page %d error = %v", recordsPerPage, err)
		}
	}
	idx, err := newIndex(t.TempDir(), testPerceptualSpec(Config{RecordsPerPage: 8}), DurabilityOS)
	if err != nil {
		t.Fatalf("minimum page size rejected: %v", err)
	}
	_ = idx.Close()
}

func assertResultsEqual(t *testing.T, got, want []Result, context uint16) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("result count = %d, want %d (context %d): got %v", len(got), len(want), context, got)
	}
	for k := range got {
		if got[k] != want[k] {
			t.Fatalf("result[%d] = %+v, want %+v (context %d)", k, got[k], want[k], context)
		}
	}
}

func slicesSortResults(results []Result) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && compareResult(results[j], results[j-1]) < 0; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}
