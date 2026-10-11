package index

import (
	"errors"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/radixhash"
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
