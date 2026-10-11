package index_test

import (
	"errors"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
)

// matchSchema binds exact (SHA-256, BLAKE3) and perceptual namespaces, plus a
// foreign-entity exact namespace for validation tests.
func matchSchema(t testing.TB) *common.Schema {
	t.Helper()
	schema := common.NewSchema()
	defs := []common.NamespaceDef{
		{Namespace: 1, Name: "time", Kind: common.KindTimestamp, Entity: common.EntityImage},
		{Namespace: 3, Name: "sim", Kind: common.KindHashPerceptual, Entity: common.EntityImage},
		{Namespace: 4, Name: "sha256", Kind: common.KindHashExact, Entity: common.EntityImage},
		{Namespace: 5, Name: "blake3", Kind: common.KindHashExact, Entity: common.EntityImage},
		{Namespace: 6, Name: "face-sha", Kind: common.KindHashExact, Entity: common.EntityFace},
	}
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatal(err)
	}
	return schema
}

func newMatcher(t *testing.T) (*index.Store, *index.Matcher) {
	t.Helper()
	db, err := index.Create(t.TempDir(), index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m, err := db.Matcher(index.MatcherSpec{SHA256: 4, BLAKE3: 5, Perceptual: 3})
	if err != nil {
		t.Fatal(err)
	}
	return db, m
}

func matchIDs(matches []index.Match) []common.ValueID {
	ids := make([]common.ValueID, 0, len(matches))
	for _, m := range matches {
		if !m.Intra {
			ids = append(ids, m.ImageID)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestMatcherExactSHA256(t *testing.T) {
	db, m := newMatcher(t)
	h := radixhash.Hash{1, 2, 3}
	if err := db.Add(index.Entry{ValueID: 100, Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(index.Entry{ValueID: 200, Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.MatchOne(nil, index.MatchItem{SHA256: &h}, index.MatchParams{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{100, 200}) {
		t.Fatalf("dup-hash matches = %v, want [100 200]", ids)
	}

	// A deleted image must not be returned.
	if err := db.Delete(common.EntityImage, 100); err != nil {
		t.Fatal(err)
	}
	got, _ = m.MatchOne(got[:0], index.MatchItem{SHA256: &h}, index.MatchParams{})
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{200}) {
		t.Fatalf("after delete = %v, want [200]", ids)
	}
}

func TestMatcherStaleHashAssociation(t *testing.T) {
	db, m := newMatcher(t)
	hA := radixhash.Hash{0xA}
	hB := radixhash.Hash{0xB}
	if err := db.Add(index.Entry{ValueID: 7, Hashes: []index.HashValue{{Namespace: 4, Hash: hA}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 7, Hashes: []index.HashValue{{Namespace: 4, Hash: hA}}},
		After:  index.Entry{ValueID: 7, Hashes: []index.HashValue{{Namespace: 4, Hash: hB}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.MatchOne(nil, index.MatchItem{SHA256: &hA}, index.MatchParams{}); len(got) != 0 {
		t.Fatalf("stale hash A returned %v", got)
	}
	got, _ := m.MatchOne(nil, index.MatchItem{SHA256: &hB}, index.MatchParams{})
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{7}) {
		t.Fatalf("hash B = %v, want [7]", ids)
	}
}

func TestMatcherExactBLAKE3(t *testing.T) {
	db, m := newMatcher(t)
	g := radixhash.Hash{9, 9}
	if err := db.Add(index.Entry{ValueID: 300, Hashes: []index.HashValue{{Namespace: 5, Hash: g}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.MatchOne(nil, index.MatchItem{BLAKE3: &g}, index.MatchParams{})
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{300}) {
		t.Fatalf("blake3 = %v, want [300]", ids)
	}
}

func TestMatcherPerceptualAndLiveness(t *testing.T) {
	db, m := newMatcher(t)
	var pdq simhash.PDQHash
	pdq[0] = 0xAB
	// simhash assigns dense 1-based IDs, so import them in order.
	if err := db.Add(index.Entry{ValueID: 1, Sims: []index.SimValue{{Namespace: 3, Record: simhash.Record{PHash: 0, PDQ: pdq, Quality: 1}}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(index.Entry{ValueID: 2, Sims: []index.SimValue{{Namespace: 3, Record: simhash.Record{PHash: 1}}}}); err != nil {
		t.Fatal(err)
	}
	// pHash distance is bitwise Hamming: 0 vs 3 is 2, so it is excluded at
	// maxDistance 1.
	if err := db.Add(index.Entry{ValueID: 3, Sims: []index.SimValue{{Namespace: 3, Record: simhash.Record{PHash: 3}}}}); err != nil {
		t.Fatal(err)
	}
	q := uint64(0)
	got, err := m.MatchOne(nil, index.MatchItem{PHash: &q}, index.MatchParams{PHashDistance: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{1, 2}) {
		t.Fatalf("pHash<=1 = %v, want [1 2]", ids)
	}
	// Native distances preserved and ascending.
	var distances []uint16
	for _, mt := range got {
		distances = append(distances, mt.Distance)
	}
	if !slices.Equal(distances, []uint16{0, 1}) {
		t.Fatalf("distances = %v, want [0 1]", distances)
	}

	// Deleted image 2 is excluded.
	if err := db.Delete(common.EntityImage, 2); err != nil {
		t.Fatal(err)
	}
	got, _ = m.MatchOne(got[:0], index.MatchItem{PHash: &q}, index.MatchParams{PHashDistance: 1})
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{1}) {
		t.Fatalf("after delete = %v, want [1]", ids)
	}

	// PDQ search returns the stored hash.
	got, _ = m.MatchOne(got[:0], index.MatchItem{PDQ: &pdq}, index.MatchParams{PDQDistance: 0})
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{1}) {
		t.Fatalf("pdq = %v, want [1]", ids)
	}
}

func TestMatcherIntraBatchDuplicates(t *testing.T) {
	_, m := newMatcher(t)
	h := radixhash.Hash{4}
	q := uint64(5)
	items := []index.MatchItem{
		{SHA256: &h, PHash: &q},
		{SHA256: &h, PHash: &q},
		{SHA256: &radixhash.Hash{6}},
	}
	got, err := m.Match(nil, items, index.MatchParams{PHashDistance: 0, DetectIntraBatch: true})
	if err != nil {
		t.Fatal(err)
	}
	var intra int
	for _, mt := range got {
		if mt.Intra {
			intra++
		}
	}
	// One SHA256 pair (two records) + one pHash pair (two records).
	if intra != 4 {
		t.Fatalf("intra matches = %d, want 4 (%v)", intra, got)
	}
}

func TestMatcherValidatesEntityAndKind(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Matcher(index.MatcherSpec{SHA256: 6}); !errors.Is(err, index.ErrMatcherNamespace) {
		t.Fatalf("foreign-entity exact err = %v, want ErrMatcherNamespace", err)
	}
	if _, err := db.Matcher(index.MatcherSpec{SHA256: 3}); !errors.Is(err, index.ErrMatcherNamespace) {
		t.Fatalf("wrong-kind err = %v, want ErrMatcherNamespace", err)
	}
	if _, err := db.Matcher(index.MatcherSpec{Perceptual: 4}); !errors.Is(err, index.ErrMatcherNamespace) {
		t.Fatalf("wrong-kind perceptual err = %v, want ErrMatcherNamespace", err)
	}
}

// TestMatcherDeletedExcludedAfterReopen verifies the conservative reopen path:
// a store reopened after a delete still filters liveness.
func TestMatcherDeletedExcludedAfterReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: matchSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	h := radixhash.Hash{7}
	if err := db.Add(index.Entry{ValueID: 100, Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(index.Entry{ValueID: 200, Hashes: []index.HashValue{{Namespace: 4, Hash: h}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(common.EntityImage, 100); err != nil {
		t.Fatal(err)
	}
	// radixhash/simhash Close does not checkpoint; Checkpoint makes the
	// committed generation durable for reopen.
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m, err := reopened.Matcher(index.MatcherSpec{SHA256: 4})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.MatchOne(nil, index.MatchItem{SHA256: &h}, index.MatchParams{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := matchIDs(got); !slices.Equal(ids, []common.ValueID{200}) {
		t.Fatalf("reopened matches = %v, want [200]", ids)
	}
}

func TestMatcherBatchOf64(t *testing.T) {
	db, m := newMatcher(t)
	items := make([]index.MatchItem, 64)
	hashes := make([]radixhash.Hash, 64)
	for i := range items {
		hashes[i] = radixhash.Hash{byte(i + 1)}
		if err := db.Add(index.Entry{ValueID: common.ValueID(1000 + i), Hashes: []index.HashValue{{Namespace: 4, Hash: hashes[i]}}}); err != nil {
			t.Fatal(err)
		}
		h := hashes[i]
		items[i] = index.MatchItem{SHA256: &h}
	}
	got, err := m.Match(nil, items, index.MatchParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 {
		t.Fatalf("batch matches = %d, want 64", len(got))
	}
	for _, mt := range got {
		if mt.ImageID != common.ValueID(1000+mt.Item) {
			t.Fatalf("item %d matched %d", mt.Item, mt.ImageID)
		}
	}
}
