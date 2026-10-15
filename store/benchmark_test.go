package store

import (
	"path/filepath"
	"testing"

	schematypes "github.com/evanoberholster/imageIdx/schema"
)

func benchStore(b *testing.B) (*Store, Key) {
	b.Helper()
	defs := schematypes.NewDefinitions()
	if err := defs.Register(schematypes.NamespaceDef{Namespace: 1, Name: "records", Kind: schematypes.KindKV, Entity: schematypes.EntityImage}); err != nil {
		b.Fatal(err)
	}
	s, err := Open(filepath.Join(b.TempDir(), "kv"), defs)
	if err != nil {
		b.Fatal(err)
	}
	return s, Key{Namespace: 1, ID: 1}
}

func BenchmarkGet(b *testing.B) {
	s, key := benchStore(b)
	defer s.Close()
	if err := s.Set(key, []byte("value")); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get(key); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSet(b *testing.B) {
	s, key := benchStore(b)
	defer s.Close()
	value := []byte("value")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Set(key, value); err != nil {
			b.Fatal(err)
		}
	}
}
