package simhash

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func sparseTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSparseExternalIDsAndReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpdates([]Update{
		{ImageID: 1, Record: Record{PHash: 10}},
		{ImageID: 2, Record: Record{PHash: 20}},
		{ImageID: 1000, Record: Record{PHash: 30}},
		{ImageID: 1<<32 + 1, Record: Record{PHash: 40}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.Len(); got != 0 {
		t.Fatalf("dense high-water = %d, want 0 for sparse batch", got)
	}
	assertSparseSearch(t, st, 10, []schema.ImageID{1})
	assertSparseSearch(t, st, 20, []schema.ImageID{2})
	assertSparseSearch(t, st, 30, []schema.ImageID{1000})
	assertSparseSearch(t, st, 40, []schema.ImageID{1<<32 + 1})

	if err := st.ApplyUpdates([]Update{{ImageID: 2, Record: Record{PHash: 21}}}); err != nil {
		t.Fatal(err)
	}
	assertSparseSearch(t, st, 21, []schema.ImageID{2})
	assertSparseSearch(t, st, 20, nil)
	if rec, ok := st.Get(2); !ok || rec.PHash != 21 {
		t.Fatalf("sparse Get = %+v %v", rec, ok)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertSparseSearch(t, st, 21, []schema.ImageID{2})
	assertSparseSearch(t, st, 30, []schema.ImageID{1000})
	assertSparseSearch(t, st, 40, []schema.ImageID{1<<32 + 1})
}

func TestSparseOverridesDenseAndMergesSorted(t *testing.T) {
	st := sparseTestStore(t)
	defer st.Close()
	if _, err := st.AppendBatch([]Record{{PHash: 1}, {PHash: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpdates([]Update{
		{ImageID: 2, Record: Record{PHash: 22}},
		{ImageID: 3, Record: Record{PHash: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	got := st.SearchPHash(nil, 1, 0)
	if len(got) != 1 || got[0].ImageID != 1 {
		t.Fatalf("pHashes = %+v", got)
	}
	got = st.SearchPHash(nil, 22, 0)
	if len(got) != 1 || got[0].ImageID != 2 || got[0].Distance != 0 {
		t.Fatalf("override = %+v", got)
	}
	got = st.SearchPHash(nil, 3, 0)
	if len(got) != 1 || got[0].ImageID != 3 {
		t.Fatalf("sparse merge = %+v", got)
	}
}

func TestSparsePDQAndTombstoneRoundTrip(t *testing.T) {
	st := sparseTestStore(t)
	defer st.Close()
	pdq := PDQHash{}
	pdq[0] = 7
	if err := st.ApplyUpdates([]Update{{ImageID: 99, Record: Record{PDQ: pdq}}}); err != nil {
		t.Fatal(err)
	}
	got := st.SearchPDQ(nil, &pdq, 0)
	if len(got) != 1 || got[0].ImageID != 99 || got[0].Distance != 0 {
		t.Fatalf("PDQ = %+v", got)
	}
	if err := st.ApplyUpdates([]Update{{ImageID: 99, Record: Record{PDQ: PDQHash{}}}}); err != nil {
		t.Fatal(err)
	}
	got = st.SearchPDQ(nil, &pdq, 0)
	if len(got) != 0 {
		t.Fatalf("old PDQ survived replacement = %+v", got)
	}
	var zero PDQHash
	got = st.SearchPDQ(nil, &zero, 0)
	if len(got) != 1 || got[0].ImageID != 99 {
		t.Fatalf("replacement zero PDQ = %+v", got)
	}
}

func assertSparseSearch(t *testing.T, st *Store, query uint64, want []schema.ImageID) {
	t.Helper()
	got := st.SearchPHash(nil, query, 0)
	if len(got) != len(want) {
		t.Fatalf("query %d: got %+v want %v", query, got, want)
	}
	for i := range want {
		if got[i].ImageID != want[i] || got[i].Distance != 0 {
			t.Fatalf("query %d result %d = %+v want %d", query, i, got[i], want[i])
		}
	}
}

func TestSparseSearchPDQBulkMatchesScalar(t *testing.T) {
	st := sparseTestStore(t)
	defer st.Close()
	pdq := func(byte0 byte) PDQHash {
		var v PDQHash
		v[0] = byte0
		return v
	}
	if _, err := st.AppendBatch([]Record{{PDQ: pdq(1)}, {PDQ: pdq(2)}, {PDQ: pdq(3)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpdates([]Update{
		{ImageID: 2, Record: Record{PDQ: pdq(20)}},
		{ImageID: 1000, Record: Record{PDQ: pdq(40)}},
		{ImageID: 1<<32 + 1, Record: Record{PDQ: pdq(80)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUpdates([]schema.ImageID{3}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query PDQHash
		max   uint8
	}{
		{query: pdq(1), max: 0},
		{query: pdq(20), max: 0},
		{query: pdq(40), max: 0},
		{query: pdq(80), max: 0},
		{query: pdq(0), max: 128},
	} {
		prefix := []Result{{ImageID: 999, Distance: 7}}
		scalar := st.SearchPDQ(append([]Result(nil), prefix...), &tc.query, tc.max)
		bulk := st.SearchPDQBulk(append([]Result(nil), prefix...), &tc.query, tc.max)
		if len(scalar) != len(bulk) {
			t.Fatalf("query %x: scalar %+v bulk %+v", tc.query[0], scalar, bulk)
		}
		for i := range scalar {
			if scalar[i] != bulk[i] {
				t.Fatalf("query %x result %d: scalar %+v bulk %+v", tc.query[0], i, scalar[i], bulk[i])
			}
		}
		if len(scalar) != 0 && scalar[0] != prefix[0] {
			t.Fatalf("prefix lost: %+v", scalar)
		}
		for i := len(prefix); i+1 < len(scalar); i++ {
			if scalar[i].ImageID >= scalar[i+1].ImageID {
				t.Fatalf("unordered results: %+v", scalar)
			}
		}
	}
	// The dense ID 3 was hidden by a sparse tombstone.
	if got := st.SearchPDQ(nil, ptrPDQ(pdq(3)), 0); len(got) != 0 {
		t.Fatalf("deleted dense ID leaked: %+v", got)
	}
	if _, ok := st.Get(3); ok {
		t.Fatal("deleted dense ID Get succeeded")
	}
	if _, ok := st.Quality(3); ok {
		t.Fatal("deleted dense ID Quality succeeded")
	}
	if q, ok := st.Quality(1000); !ok || q != 0 {
		t.Fatalf("sparse quality = %d %v", q, ok)
	}
}

func ptrPDQ(v PDQHash) *PDQHash { return &v }

func TestSparseReplayGenerationAndCompaction(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 10}}}, 7); err != nil {
		t.Fatal(err)
	}
	gen := st.SparseGeneration()
	if err := st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 20}}}, 7); err != nil {
		t.Fatal(err)
	}
	if st.SparseGeneration() != gen || st.SparseSourceGeneration() != 7 {
		t.Fatalf("replayed interval advanced generation: %d/%d", st.SparseGeneration(), st.SparseSourceGeneration())
	}
	assertSparseSearch(t, st, 10, []schema.ImageID{1000})
	if err := st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 999}}}, 8); err != nil {
		t.Fatal(err)
	}
	assertSparseSearch(t, st, 999, []schema.ImageID{1000})
	for i := 0; i < maxSparseSegments; i++ {
		id := schema.ImageID(1<<32 + i + 2)
		if err := st.ApplyUpdatesAt([]Update{{ImageID: id, Record: Record{PHash: uint64(i + 1)}}}, uint64(i+9)); err != nil {
			t.Fatal(err)
		}
		if st.SparseSegmentCount() > maxSparseSegments {
			t.Fatalf("segments = %d", st.SparseSegmentCount())
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertSparseSearch(t, st, 999, []schema.ImageID{1000})
	if st.SparseSourceGeneration() != 8+maxSparseSegments {
		t.Fatalf("source generation = %d", st.SparseSourceGeneration())
	}
}

func TestSparseLegacyAndOrphanHandling(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sparse.bin"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Config{}); !errors.Is(err, ErrUnsupportedSparseFormat) {
		t.Fatalf("legacy sparse: %v", err)
	}

	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sparse-0000000000000001.seg"), []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sparse-0000000000000001.seg")); !os.IsNotExist(err) {
		t.Fatalf("orphan was not removed: %v", err)
	}
}

func TestSparsePublicationCrashBoundaries(t *testing.T) {
	stages := []struct {
		name     string
		newState bool
	}{
		{"segment-before", false},
		{"segment-before-sync", false},
		{"segment-before-rename", false},
		{"segment-after-rename", false},
		{"manifest-before", false},
		{"manifest-before-rename", false},
		{"manifest-after-rename", true},
		{"before-publish", true},
	}
	for _, tc := range stages {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := Open(dir, Config{Durability: schema.DurabilitySync})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 10}}}, 1); err != nil {
				t.Fatal(err)
			}
			sparseFail = func(stage string) error {
				if stage == tc.name {
					return errSparseCrash
				}
				return nil
			}
			err = st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 20}}}, 2)
			sparseFail = nil
			if err == nil {
				t.Fatal("crash seam did not fire")
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(dir, Config{Durability: schema.DurabilitySync})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if tc.newState {
				assertSparseSearch(t, st, 20, []schema.ImageID{1000})
				if st.SparseSourceGeneration() != 2 {
					t.Fatalf("source generation = %d", st.SparseSourceGeneration())
				}
			} else {
				assertSparseSearch(t, st, 10, []schema.ImageID{1000})
				if st.SparseSourceGeneration() != 1 {
					t.Fatalf("source generation = %d", st.SparseSourceGeneration())
				}
			}
		})
	}
}

func TestSparseCompactionCrashBoundary(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxSparseSegments; i++ {
		if err := st.ApplyUpdatesAt([]Update{{ImageID: schema.ImageID(1<<32 + i), Record: Record{PHash: uint64(i)}}}, uint64(i)); err != nil {
			t.Fatal(err)
		}
	}
	sparseFail = func(stage string) error {
		if stage == "manifest-after-rename" {
			return errSparseCrash
		}
		return nil
	}
	err = st.ApplyUpdatesAt([]Update{{ImageID: 1<<32 + maxSparseSegments, Record: Record{PHash: maxSparseSegments}}}, maxSparseSegments)
	sparseFail = nil
	if err == nil {
		t.Fatal("compaction crash seam did not fire")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 1; i <= maxSparseSegments; i++ {
		assertSparseSearch(t, st, uint64(i), []schema.ImageID{schema.ImageID(1<<32 + i)})
	}
	if st.SparseSegmentCount() > maxSparseSegments {
		t.Fatalf("segments = %d", st.SparseSegmentCount())
	}
}

var errSparseCrash = errors.New("injected sparse crash")

func TestSparseCorruptionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 7}}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "sparse.manifest")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(manifest, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Config{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest: %v", err)
	}

	// Restore a valid manifest and damage a referenced segment. A complete but
	// corrupt segment must fail closed rather than being truncated.
	dir = t.TempDir()
	st, err = Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyUpdatesAt([]Update{{ImageID: 1000, Record: Record{PHash: 7}}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "sparse-") && strings.HasSuffix(entry.Name(), ".seg") {
			path := filepath.Join(dir, entry.Name())
			segment, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			segment[len(segment)-1] ^= 1
			if err := os.WriteFile(path, segment, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := Open(dir, Config{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt segment: %v", err)
	}
}

func TestSparseDenseTombstoneSurvivesCompaction(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendBatch([]Record{{PHash: 1}, {PHash: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUpdates([]schema.ImageID{1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSparseSegments; i++ {
		id := schema.ImageID(1<<32 + i + 1)
		if err := st.ApplyUpdatesAt([]Update{{ImageID: id, Record: Record{PHash: uint64(i + 10)}}}, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.SearchPHash(nil, 1, 0); len(got) != 0 {
		t.Fatalf("deleted dense record survived compaction: %+v", got)
	}
	if _, ok := st.Get(1); ok {
		t.Fatal("deleted dense Get succeeded")
	}
	if _, ok := st.Quality(1); ok {
		t.Fatal("deleted dense Quality succeeded")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir, Config{Durability: schema.DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := st.SearchPHash(nil, 1, 0); len(got) != 0 {
		t.Fatalf("reopened deleted dense record: %+v", got)
	}
}
