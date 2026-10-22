package store

import (
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
