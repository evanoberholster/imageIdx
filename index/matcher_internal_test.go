package index

import (
	"errors"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
)

// P1-1: a namespace degraded after the matcher is constructed must be rejected
// for the operations that need it, while healthy namespaces keep working.
func TestMatcherRejectsDegradedNamespace(t *testing.T) {
	schema := common.NewSchema()
	if err := schema.Register(common.NamespaceDef{Namespace: 1, Name: "sha", Kind: common.KindHashExact, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(common.NamespaceDef{Namespace: 2, Name: "sim", Kind: common.KindHashPerceptual, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	db, err := Create(t.TempDir(), Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.Matcher(MatcherSpec{SHA256: 1, Perceptual: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Degrade the exact namespace after construction.
	db.mu.Lock()
	db.markDegraded(1, errors.New("boom"))
	db.mu.Unlock()

	h := radixhash.Hash{1}
	if _, err := m.MatchOne(nil, MatchItem{SHA256: &h}, MatchParams{}); !errors.Is(err, ErrNamespaceUnavailable) {
		t.Fatalf("degraded exact err = %v, want ErrNamespaceUnavailable", err)
	}
	// A perceptual-only query must still succeed.
	q := uint64(1)
	if _, err := m.MatchOne(nil, MatchItem{PHash: &q}, MatchParams{PHashDistance: 0}); err != nil {
		t.Fatalf("perceptual match after exact degradation = %v", err)
	}
}

// P0-3: when one required namespace is degraded, a mixed batch must fail
// before appending any results, leaving the destination unchanged.
func TestMatcherNoPartialResultsOnDegraded(t *testing.T) {
	schema := common.NewSchema()
	if err := schema.Register(common.NamespaceDef{Namespace: 1, Name: "sha", Kind: common.KindHashExact, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(common.NamespaceDef{Namespace: 2, Name: "sim", Kind: common.KindHashPerceptual, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	db, err := Create(t.TempDir(), Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := radixhash.Hash{1}
	if err := db.Add(Entry{ValueID: 1, Hashes: []HashValue{{Namespace: 1, Hash: h}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(Entry{ValueID: 1, Sims: []SimValue{{Namespace: 2, Record: simhash.Record{PHash: 0}}}}); err != nil {
		t.Fatal(err)
	}
	m, err := db.Matcher(MatcherSpec{SHA256: 1, Perceptual: 2})
	if err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	db.markDegraded(2, errors.New("boom"))
	db.mu.Unlock()

	dst := make([]Match, 0, 8)
	items := []MatchItem{{SHA256: &h}, {PHash: new(uint64)}}
	if _, err := m.Match(dst, items, MatchParams{PHashDistance: 0}); !errors.Is(err, ErrNamespaceUnavailable) {
		t.Fatalf("mixed degraded err = %v, want ErrNamespaceUnavailable", err)
	}
	if len(dst) != 0 {
		t.Fatalf("partial results returned despite degradation: %v", dst)
	}
}
