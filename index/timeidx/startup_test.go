package timeidx

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestImmutableStartupRegistry(t *testing.T) {
	s, results, err := InitializeStore(t.TempDir(), []NamespaceInit{{Spec: Spec{Namespace: 3}, Create: true}, {Spec: Spec{Namespace: 1}, Create: true}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, r := range results {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	if s.indexes[0].namespace != 1 || s.indexes[1].namespace != 3 {
		t.Fatal("unsorted")
	}
	if _, err = s.CreateNamespace(2); err != ErrNamespaceFrozen {
		t.Fatal(err)
	}
	if err = s.CloseNamespace(1); err != ErrNamespaceFrozen {
		t.Fatal(err)
	}
	if _, err = s.OpenNamespace(2); err != ErrNamespaceFrozen {
		t.Fatal(err)
	}
	a, ok := s.Namespace(1)
	if !ok {
		t.Fatal("lookup")
	}
	if alloc := testing.AllocsPerRun(100, func() {
		if _, ok := s.Namespace(1); !ok {
			t.Fatal("lookup")
		}
	}); alloc != 0 {
		t.Fatal(alloc)
	}
	token := a.readers.Acquire()
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	for !s.closed.Load() {
	} // publication precedes draining readers
	if _, ok := s.Namespace(3); ok {
		t.Fatal("shutdown lookup")
	}
	b := s.indexes[1].index
	if err := b.Apply(NewBatch(0)); err != ErrClosed {
		t.Fatal("shutdown admitted write", err)
	}
	a.readers.Release(token)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredCorruptionIsolation(t *testing.T) {
	dir := t.TempDir()
	i, err := NewStore(dir, Spec{Namespace: 1}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	i.Close()
	if err = os.WriteFile(filepath.Join(dir, NamespaceFilename(1)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	s, results, err := InitializeStore(dir, []NamespaceInit{{Spec: Spec{Namespace: 1}}, {Spec: Spec{Namespace: 2}, Create: true}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !errors.Is(results[0].Err, ErrRebuildRequired) || !errors.Is(s.NamespaceError(1), ErrCorrupt) {
		t.Fatal(results)
	}
	if _, ok := s.Namespace(1); ok {
		t.Fatal("corrupt index available")
	}
	i, ok := s.Namespace(2)
	if !ok {
		t.Fatal("healthy index blocked")
	}
	apply(t, i, Insert(1, 1))
	if _, _, err := InitializeStore(t.TempDir(), []NamespaceInit{{Spec: Spec{Namespace: 1}}, {Spec: Spec{Namespace: 1}}}, Config{}, DurabilitySync); err != ErrNamespaceExists {
		t.Fatal(err)
	}
}

func TestConcurrentSealedLookup(t *testing.T) {
	s, _, err := InitializeStore(t.TempDir(), []NamespaceInit{{Spec: Spec{Namespace: 1}, Create: true}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 1000; n++ {
				if i, ok := s.Namespace(1); ok {
					i.Search(nil, 0, 1)
				}
			}
		}()
	}
	s.Close()
	wg.Wait()
}

func TestRebuildClassification(t *testing.T) {
	for _, err := range []error{ErrUnsupported, ErrInvalidConfig, os.ErrPermission, os.ErrNotExist} {
		if errors.Is(rebuildError(err), ErrRebuildRequired) {
			t.Fatal(err)
		}
	}
	for _, err := range []error{ErrCorrupt, ErrNamespaceMismatch} {
		if !errors.Is(rebuildError(err), ErrRebuildRequired) {
			t.Fatal(err)
		}
	}
}
