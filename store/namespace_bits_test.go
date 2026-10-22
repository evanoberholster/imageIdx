package store

import (
	"errors"
	"fmt"
	"testing"
	"unsafe"

	schematypes "github.com/evanoberholster/imageIdx/schema"
)

func TestKVNamespaceBitsSize(t *testing.T) {
	if got := unsafe.Sizeof(kvNamespaceBits{}); got != 8<<10 {
		t.Fatalf("kvNamespaceBits size = %d, want %d", got, 8<<10)
	}
}

func TestKVNamespaceBitsBoundaries(t *testing.T) {
	var bits kvNamespaceBits
	for _, ns := range []schematypes.Namespace{0, 1, 63, 64, 0xFFF0, 0xFFFE, 0xFFFF} {
		if bits.contains(ns) {
			t.Fatalf("empty bitmap contains %d", ns)
		}
		bits.add(ns)
		if !bits.contains(ns) {
			t.Fatalf("bitmap missing %d after add", ns)
		}
	}
	for _, ns := range []schematypes.Namespace{2, 65, 0xFFEF} {
		if bits.contains(ns) {
			t.Fatalf("bitmap unexpectedly contains %d", ns)
		}
	}
}

func TestKVNamespaceBitsMatchesDefinitions(t *testing.T) {
	defs := schematypes.NewDefinitions()
	for _, ns := range []schematypes.Namespace{1, 2, 100, 0xFFF0, 0xFFFD, 0xFFFE, 0xFFFF} {
		if err := defs.Register(schematypes.NamespaceDef{
			Namespace: ns,
			Name:      fmt.Sprintf("ns-%d", ns),
			Kind:      schematypes.KindKV,
			Entity:    schematypes.EntityImage,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var bits kvNamespaceBits
	for _, def := range defs.Defs() {
		if def.Kind != schematypes.KindKV {
			t.Fatalf("unexpected kind for %d", def.Namespace)
		}
		bits.add(def.Namespace)
	}
	for _, ns := range []schematypes.Namespace{1, 2, 100, 0xFFF0, 0xFFFD, 0xFFFE, 0xFFFF} {
		if !bits.contains(ns) {
			t.Fatalf("bitmap missing registered namespace %d", ns)
		}
	}
	if bits.contains(0) || bits.contains(3) {
		t.Fatal("bitmap contains an unregistered namespace")
	}
}

func benchmarkNamespaceMembership(b *testing.B, count int) []schematypes.Namespace {
	b.Helper()
	namespaces := make([]schematypes.Namespace, count)
	for i := range namespaces {
		namespaces[i] = schematypes.Namespace(i*97 + 1)
	}
	return namespaces
}

func BenchmarkKVNamespaceLookupMap(b *testing.B) {
	for _, count := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("namespaces=%d", count), func(b *testing.B) {
			namespaces := benchmarkNamespaceMembership(b, count)
			m := make(map[schematypes.Namespace]struct{}, len(namespaces))
			for _, ns := range namespaces {
				m[ns] = struct{}{}
			}
			target := namespaces[len(namespaces)-1]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := m[target]; !ok {
					b.Fatal("missing")
				}
			}
		})
	}
}

func BenchmarkKVNamespaceLookupBitmap(b *testing.B) {
	for _, count := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("namespaces=%d", count), func(b *testing.B) {
			namespaces := benchmarkNamespaceMembership(b, count)
			var bits kvNamespaceBits
			for _, ns := range namespaces {
				bits.add(ns)
			}
			target := namespaces[len(namespaces)-1]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !bits.contains(target) {
					b.Fatal("missing")
				}
			}
		})
	}
}

func TestOpenBitmapMembership(t *testing.T) {
	dir := t.TempDir()
	defs := schematypes.NewDefinitions()
	namespaces := []schematypes.Namespace{1, 63, 64, 0xFFF0, 0xFFFE, 0xFFFF}
	for _, ns := range namespaces {
		if err := defs.Register(schematypes.NamespaceDef{
			Namespace: ns,
			Name:      fmt.Sprintf("kv-%d", ns),
			Kind:      schematypes.KindKV,
			Entity:    schematypes.EntityImage,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(dir, defs)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range namespaces {
		if err := s.Set(Key{Namespace: ns, ID: 1}, []byte{byte(ns)}); err != nil {
			t.Fatalf("namespace %d set: %v", ns, err)
		}
		if got, err := s.Get(Key{Namespace: ns, ID: 1}); err != nil || len(got) != 1 {
			t.Fatalf("namespace %d get: %v %v", ns, got, err)
		}
	}
	if err := s.Set(Key{Namespace: 2, ID: 1}, []byte("x")); !errors.Is(err, ErrNamespace) {
		t.Fatalf("unknown namespace: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, defs)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ns := range namespaces {
		if got, err := s.Get(Key{Namespace: ns, ID: 1}); err != nil || len(got) != 1 {
			t.Fatalf("reopen namespace %d: %v %v", ns, got, err)
		}
	}
}

func TestOpenRejectsNonKVDefinition(t *testing.T) {
	defs := schematypes.NewDefinitions()
	if err := defs.Register(schematypes.NamespaceDef{
		Namespace: 7, Name: "index", Kind: schematypes.KindTimestamp, Entity: schematypes.EntityImage,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.TempDir(), defs); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("non-KV definition: %v", err)
	}
}

func TestOpenLegacyBitmapMembership(t *testing.T) {
	s, err := OpenLegacy(t.TempDir(), Spec{Namespace: 1, Name: "one"}, Spec{Namespace: 0xFFFF, Name: "last"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Set(Key{Namespace: 0xFFFF, ID: 1}, []byte("last")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Key{Namespace: 0, ID: 1}, []byte("zero")); !errors.Is(err, ErrNamespace) {
		t.Fatalf("reserved zero namespace: %v", err)
	}
}
