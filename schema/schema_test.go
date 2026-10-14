package schema

import (
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
)

func testOptions(backend Backend) Options {
	return Options{Backend: backend, Namespaces: []NamespaceConfig{
		{ID: common.NamespaceTag, Name: "Tag", ReserveSize: 8},
		{ID: common.NamespacePerson, Name: "Person", ReserveSize: 4},
	}}
}

func forBackends(t *testing.T, run func(t *testing.T, backend Backend)) {
	for _, b := range []Backend{BackendReadWrite, BackendMmap} {
		name := "readwrite"
		if b == BackendMmap {
			name = "mmap"
		}
		t.Run(name, func(t *testing.T) { run(t, b) })
	}
}

func TestAllocationAndCleanReopen(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if other, err := Open(path, opts); !errors.Is(err, ErrLocked) {
			if other != nil {
				other.Close()
			}
			t.Fatalf("second owner: %v", err)
		}
		if id, err := s.NextID(common.NamespaceTag); id != 1 || err != nil {
			t.Fatalf("first tag: %d %v", id, err)
		}
		if id, err := s.NextID(common.NamespacePerson); id != 1 || err != nil {
			t.Fatalf("first person: %d %v", id, err)
		}
		if start, err := s.NextIDs(common.NamespaceTag, 17); start != 2 || err != nil {
			t.Fatalf("cross blocks: %d %v", start, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(common.NamespaceTag); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		opts.Namespaces[0].ReserveSize = 13
		opts.Namespaces = append(opts.Namespaces, NamespaceConfig{ID: 256, Name: "Image", ReserveSize: 16})
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(common.NamespaceTag); id != 19 || err != nil {
			t.Fatalf("clean checkpoint: %d %v", id, err)
		}
		if id, err := s.NextID(256); id != 1 || err != nil {
			t.Fatalf("new namespace: %d %v", id, err)
		}
		if err := s.EnsureMinimum(common.NamespacePerson, 50000); err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(common.NamespacePerson); id != 50001 || err != nil {
			t.Fatalf("minimum: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if id, err := s.NextID(common.NamespacePerson); id != 50002 || err != nil {
			t.Fatalf("second clean: %d %v", id, err)
		}
	})
}

func TestConfigurationAndOverflow(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := testOptions(b)
		for _, bad := range []Options{
			{Backend: 9, Namespaces: opts.Namespaces},
			{Backend: b, Namespaces: []NamespaceConfig{{ID: 0, Name: "zero"}}},
			{Backend: b, Namespaces: []NamespaceConfig{{ID: 1, Name: "X"}, {ID: 1, Name: "Y"}}},
			{Backend: b, Namespaces: []NamespaceConfig{{ID: 1, Name: "X"}, {ID: 2, Name: "X"}}},
		} {
			if s, err := Open(path, bad); err == nil {
				s.Close()
				t.Fatal("accepted invalid config")
			}
		}
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.NextID(0); !errors.Is(err, ErrNamespace) {
			t.Fatal(err)
		}
		if _, err := s.NextIDs(common.NamespaceTag, 0); !errors.Is(err, ErrCount) {
			t.Fatal(err)
		}
		if err := s.EnsureMinimum(common.NamespaceTag, math.MaxUint64-1); err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(common.NamespaceTag); id != math.MaxUint64 || err != nil {
			t.Fatalf("last ID: %d %v", id, err)
		}
		if id, err := s.NextID(common.NamespaceTag); id != 0 || !errors.Is(err, ErrExhausted) {
			t.Fatalf("overflow: %d %v", id, err)
		}
		if id, err := s.NextIDs(common.NamespacePerson, math.MaxUint64); id != 1 || err != nil {
			t.Fatalf("max range: %d %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wrong := testOptions(b)
		wrong.Namespaces[0].Name = "renamed"
		if _, err := Open(path, wrong); !errors.Is(err, ErrNamespace) {
			t.Fatalf("renamed: %v", err)
		}
		wrong = testOptions(b)
		wrong.Namespaces = wrong.Namespaces[:1]
		if _, err := Open(path, wrong); !errors.Is(err, ErrNamespace) {
			t.Fatalf("removed namespace: %v", err)
		}
	})
}
