package index

import (
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/index/simhash"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestUnifiedSparseSimHashExternalIDs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")
	defs := schema.NewDefinitions()
	if err := defs.Register(schema.NamespaceDef{
		Namespace: 3, Name: "perceptual", Kind: schema.KindHashPerceptual, Entity: schema.EntityImage,
	}); err != nil {
		t.Fatal(err)
	}
	store, err := Create(dir, Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Entry{ValueID: 1000, Sims: []SimValue{{Namespace: 3, Record: simhash.Record{PHash: 42}}}}); err != nil {
		t.Fatal(err)
	}
	sim, ok := store.Sim(3)
	if !ok {
		t.Fatal("simhash missing")
	}
	if got := sim.SearchPHash(nil, 42, 0); len(got) != 1 || got[0].ImageID != 1000 {
		t.Fatalf("sparse search = %+v", got)
	}
	if err := store.Add(Entry{ValueID: 1000, Sims: []SimValue{{Namespace: 3, Record: simhash.Record{PHash: 43}}}}); err != nil {
		t.Fatal(err)
	}
	if got := sim.SearchPHash(nil, 42, 0); len(got) != 0 {
		t.Fatalf("stale sparse search = %+v", got)
	}
	if got := sim.SearchPHash(nil, 43, 0); len(got) != 1 || got[0].ImageID != 1000 {
		t.Fatalf("updated sparse search = %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sim, ok = store.Sim(3)
	if !ok {
		t.Fatal("reopened simhash missing")
	}
	if got := sim.SearchPHash(nil, 43, 0); len(got) != 1 || got[0].ImageID != 1000 {
		t.Fatalf("reopened sparse search = %+v", got)
	}
}
