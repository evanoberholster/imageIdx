package store

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestStatsTrackPhysicalWrites(t *testing.T) {
	dir := t.TempDir()
	defs := schema.NewDefinitions()
	if err := defs.Register(schema.NamespaceDef{
		Namespace: 100, Name: "stats", Kind: schema.KindKV, Entity: schema.EntityImage,
	}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, defs)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Set(Key{Namespace: 100, ID: 1}, []byte("value")); err != nil {
		t.Fatal(err)
	}
	b := s.NewBatch()
	if err := b.Set(Key{Namespace: 100, ID: 2}, []byte("batch")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(Key{Namespace: 100, ID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	stats := s.Stats()
	if stats.Commits != 2 || stats.Sets != 2 || stats.Deletes != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.KeyBytes == 0 || stats.ValueBytes == 0 {
		t.Fatalf("byte stats = %+v", stats)
	}
}
