package simhash

import (
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
