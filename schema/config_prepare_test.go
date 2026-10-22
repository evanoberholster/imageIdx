package schema

import (
	"path/filepath"
	"testing"
)

func TestSchemaConfigSurvivesPreparedProjectionVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.dat")
	defs := NewDefinitions()
	if err := defs.Register(NamespaceDef{Namespace: 9, Name: "kv", Kind: KindKV, Entity: EntityImage}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, Options{Definitions: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfig([6]byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	target := NewDefinitions()
	def, ok := s.Definitions().Lookup(9)
	if !ok {
		t.Fatal("definition missing")
	}
	def.ProjectionVersion = 2
	if err := target.Register(def); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{Definitions: target, Prepare: true})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen prepared: %v", err)
	}
	defer s.Close()
	if got := s.Config(); got != ([6]byte{1, 2}) {
		t.Fatalf("config = %v", got)
	}
	if s.PreparedDefinitions() == nil {
		t.Fatal("prepared target not recovered")
	}
	if def, _ := s.PreparedDefinitions().Lookup(9); def.ProjectionVersion != 2 {
		t.Fatalf("prepared projection version = %d", def.ProjectionVersion)
	}
}
