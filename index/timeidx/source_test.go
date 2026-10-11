package timeidx

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

func sourceApply(t testing.TB, i *Index, g uint64, ops ...Mutation) {
	t.Helper()
	b := NewBatch(len(ops))
	for _, op := range ops {
		b.Append(op)
	}
	if err := i.ApplyWithGeneration(b, g); err != nil {
		t.Fatal(err)
	}
}

func TestSourceGenerationAtomicPersistence(t *testing.T) {
	i := persistent(t, Config{})
	sourceApply(t, i, 7, Insert(1, 42))
	gen := i.Generation()
	if i.SourceGeneration() != 7 || i.CheckSourceGeneration(8) != ErrIndexStale {
		t.Fatal("watermark")
	}
	sourceApply(t, i, 7, Insert(1, 42))
	if i.Generation() != gen {
		t.Fatal("retry rewrote")
	}
	if err := i.ApplyWithGeneration(NewBatch(0), 6); err != ErrSourceGeneration {
		t.Fatal(err)
	}
	sourceApply(t, i, 8) // empty checkpoint must persist
	apply(t, i, Insert(2, 43))
	if i.SourceGeneration() != 8 {
		t.Fatal("Apply invented checkpoint")
	}
	sourceApply(t, i, 9, Insert(2, 43)) // physical no-op still advances
	dir := filepath.Dir(i.path)
	i.Close()
	j, err := OpenNamespace(dir, 1, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if j.SourceGeneration() != 9 || j.Len() != 2 || j.CheckSourceGeneration(10) != ErrIndexStale {
		t.Fatal(j.SourceGeneration(), j.Len())
	}
}

func TestSourceGenerationCommitFailures(t *testing.T) {
	for _, stage := range []string{"write", "data-sync", "commit", "partial-root", "root-sync"} {
		t.Run(stage, func(t *testing.T) {
			i := persistent(t, Config{})
			sourceApply(t, i, 1, Insert(1, 1))
			i.fault = func(s string) error {
				if s == stage {
					return errors.New("fault")
				}
				return nil
			}
			b := NewBatch(1)
			b.Append(Insert(2, 2))
			if err := i.ApplyWithGeneration(b, 2); err == nil {
				t.Fatal("acknowledged failure")
			}
			if i.SourceGeneration() != 1 || i.Len() != 1 {
				t.Fatal("partial publication")
			}
			dir := filepath.Dir(i.path)
			i.Close()
			j, err := OpenNamespace(dir, 1, Config{}, DurabilitySync)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			if j.SourceGeneration() != j.Len() || (j.Len() != 1 && j.Len() != 2) {
				t.Fatal("incoherent root", j.SourceGeneration(), j.Len())
			}
		})
	}
}

func TestEmptyCheckpointOnlyReopen(t *testing.T) {
	i := persistent(t, Config{})
	sourceApply(t, i, 42)
	dir := filepath.Dir(i.path)
	i.Close()
	j, err := OpenNamespace(dir, 1, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if j.SourceGeneration() != 42 || j.Len() != 0 {
		t.Fatal(j.SourceGeneration(), j.Len())
	}
}

func TestIndependentNamespaceSourceGenerations(t *testing.T) {
	dir := t.TempDir()
	s, results, err := InitializeStore(dir, []NamespaceInit{{Spec: Spec{Namespace: 1}, Create: true}, {Spec: Spec{Namespace: 2}, Create: true}}, Config{}, DurabilitySync)
	if err != nil || results[0].Err != nil || results[1].Err != nil {
		t.Fatal(err, results)
	}
	a, _ := s.Namespace(1)
	b, _ := s.Namespace(2)
	sourceApply(t, a, 7, Insert(1, 1))
	sourceApply(t, b, 42, Insert(1, 2))
	if a.SourceGeneration() != 7 || b.SourceGeneration() != 42 {
		t.Fatal("shared checkpoint")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, results, err = InitializeStore(dir, []NamespaceInit{{Spec: Spec{Namespace: 1}, RequiredSourceGeneration: 7}, {Spec: Spec{Namespace: 2}, RequiredSourceGeneration: 42}}, Config{}, DurabilitySync)
	if err != nil || results[0].Err != nil || results[1].Err != nil {
		t.Fatal(err, results)
	}
	defer s.Close()
	a, _ = s.Namespace(1)
	b, _ = s.Namespace(2)
	if a.SourceGeneration() != 7 || b.SourceGeneration() != 42 {
		t.Fatal("shared recovered checkpoint")
	}
}

func TestMixedInitializationStates(t *testing.T) {
	dir := t.TempDir()
	for _, ns := range []common.Namespace{1, 3, 4} {
		i, err := NewStore(dir, Spec{Namespace: ns}, Config{}, DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		sourceApply(t, i, 1, Insert(1, 1))
		i.Close()
	}
	if err := os.WriteFile(filepath.Join(dir, NamespaceFilename(3)), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	s, rs, err := InitializeStore(dir, []NamespaceInit{{Spec: Spec{Namespace: 1}, RequiredSourceGeneration: 1}, {Spec: Spec{Namespace: 2}}, {Spec: Spec{Namespace: 3}}, {Spec: Spec{Namespace: 4}, RequiredSourceGeneration: 2}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if rs[0].Err != nil || !errors.Is(rs[1].Err, ErrNamespaceNotFound) || !errors.Is(rs[2].Err, ErrRebuildRequired) || !errors.Is(rs[3].Err, ErrIndexStale) {
		t.Fatal(rs)
	}
	if errors.Is(rs[3].Err, ErrCorrupt) {
		t.Fatal("stale classified corrupt")
	}
	i, ok := s.Namespace(1)
	if !ok || len(i.Search(nil, 0, 2)) != 1 {
		t.Fatal("healthy blocked")
	}
}

func TestVersion4Rejected(t *testing.T) {
	i := persistent(t, Config{})
	path := i.path
	i.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for slot := 0; slot < 2; slot++ {
		b := data[slot*512 : (slot+1)*512]
		h, p, ok := storage.DecodeSuperblock(b, fileMagic, fileVersion)
		if ok {
			h.Version = 4
			if err := storage.EncodeSuperblock(b, h, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenNamespace(filepath.Dir(path), 1, Config{}, DurabilitySync); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestConcurrentSourceOrderingAndAtomicVisibility(t *testing.T) {
	i := persistent(t, Config{})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	i.crash = func(stage string) {
		if stage == "commit" {
			once.Do(func() { close(entered); <-release })
		}
	}
	b := NewBatch(2)
	b.Append(Insert(1, 1))
	b.Append(Insert(2, 2))
	done := make(chan error, 1)
	go func() { done <- i.ApplyWithGeneration(b, 2) }()
	<-entered
	if i.SourceGeneration() != 0 || i.Len() != 0 {
		t.Fatal("checkpoint before data publication")
	}
	lower := make(chan error, 1)
	go func() { one := NewBatch(1); one.Append(Insert(1, 1)); lower <- i.ApplyWithGeneration(one, 1) }()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-lower; err != ErrSourceGeneration {
		t.Fatal(err)
	}
	if i.SourceGeneration() != 2 || i.Len() != 2 {
		t.Fatal("checkpoint incoherent")
	}
	i.crash = nil
}
