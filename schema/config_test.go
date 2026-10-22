package schema

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSchemaConfigPersistsAndMismatchFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	defs := NewDefinitions()
	if err := defs.Register(NamespaceDef{Namespace: 9, Name: "kv", Kind: KindKV, Entity: EntityImage}); err != nil {
		t.Fatal(err)
	}
	config := [6]byte{1, 2}
	s, err := Open(path, Options{Definitions: defs, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config(); got != config {
		t.Fatalf("config = %v", got)
	}
	if err := s.SetConfig([6]byte{1, 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config(); got != ([6]byte{1, 1}) {
		t.Fatalf("reopened config = %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{Config: [6]byte{1, 2}}); !errors.Is(err, ErrConfigMismatch) {
		t.Fatalf("mismatched config: %v", err)
	}
}
