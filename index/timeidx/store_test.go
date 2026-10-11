package timeidx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/common"
)

func TestNamespaceManagementAndIsolation(t *testing.T) {
	dir := t.TempDir()
	s, err := NewNamespaceStore(dir, Config{MaxShardBytes: 256}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.CreateNamespace(1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateNamespace(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNamespace(1); !errors.Is(err, ErrNamespaceExists) {
		t.Fatal(err)
	}
	if _, err := s.OpenNamespace(3); err != ErrNamespaceNotFound {
		t.Fatal(err)
	}
	if _, err := s.CreateNamespace(0); err != ErrInvalidNamespace {
		t.Fatal(err)
	}
	apply(t, a, Insert(7, 42))
	apply(t, b, Insert(7, 42))
	bg := b.Generation()
	bstats := b.OnDiskBytes()
	apply(t, a, Move(7, 42, 43), Insert(8, 42))
	if b.Generation() != bg || b.OnDiskBytes() != bstats || !slices.Equal(b.Search(nil, 42, 43), []uint64{7}) {
		t.Fatal("isolation")
	}
	if err := s.CloseNamespace(1); err != nil {
		t.Fatal(err)
	}
	apply(t, b, Insert(9, 43))
	if a.Apply(NewBatch(0)) != ErrClosed {
		t.Fatal("closed index")
	}
	a, err = s.OpenNamespace(1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.Search(nil, 42, 44), []uint64{8, 7}) {
		t.Fatal(a.Search(nil, 42, 44))
	}
	var wg sync.WaitGroup
	for n := 0; n < 10; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i, err := s.OpenNamespace(1)
			if err != nil || i != a {
				t.Error("duplicate mapping", err)
			}
		}()
	}
	wg.Wait()
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	if !slices.Equal(names, []string{"0001.timeidx", "0001.timeidx.lock", "0002.timeidx", "0002.timeidx.lock"}) {
		t.Fatal(names)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenNamespace(1); err != ErrClosed {
		t.Fatal(err)
	}
}

func TestConcurrentNamespaceCreationAndWrites(t *testing.T) {
	s, err := NewNamespaceStore(t.TempDir(), Config{MaxShardBytes: 512}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	var wins int
	var mu sync.Mutex
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateNamespace(1)
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrNamespaceExists) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatal(wins)
	}
	for ns := common.Namespace(2); ns < 5; ns++ {
		if _, err := s.CreateNamespace(ns); err != nil {
			t.Fatal(err)
		}
	}
	for ns := common.Namespace(1); ns < 5; ns++ {
		wg.Add(1)
		go func(ns common.Namespace) {
			defer wg.Done()
			i, err := s.OpenNamespace(ns)
			if err != nil {
				t.Error(err)
				return
			}
			for n := uint64(0); n < 10; n++ {
				b := NewBatch(1)
				b.Append(Insert(n, n))
				if err := i.Apply(b); err != nil {
					t.Error(err)
				}
			}
		}(ns)
	}
	wg.Wait()
	for ns := common.Namespace(1); ns < 5; ns++ {
		i, _ := s.OpenNamespace(ns)
		if i.Len() != 10 {
			t.Fatal(ns, i.Len())
		}
	}
}

func TestNamespaceIdentityAndLegacyRejection(t *testing.T) {
	dir := t.TempDir()
	i, err := NewStore(dir, Spec{Namespace: 1}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	i.Close()
	if err := os.Rename(filepath.Join(dir, NamespaceFilename(1)), filepath.Join(dir, NamespaceFilename(2))); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenNamespace(dir, 2, Config{}, DurabilitySync); err != ErrNamespaceMismatch {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, "manifest"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(legacy, Config{}, DurabilitySync); err != ErrUnsupported {
		t.Fatal(err)
	}
}

func TestNamespaceReaderAndFailureIsolation(t *testing.T) {
	dir := t.TempDir()
	s, err := NewNamespaceStore(dir, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.CreateNamespace(1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateNamespace(2)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, a, Insert(1, 1))
	apply(t, b, Insert(1, 1))
	token := a.readers.Acquire()
	pinned := a.view.Load()
	waiting := make(chan struct{})
	done := make(chan error, 1)
	a.crash = func(stage string) {
		if stage == "reclaim" {
			close(waiting)
		}
	}
	go func() { batch := NewBatch(1); batch.Append(Move(1, 1, 2)); done <- a.Apply(batch) }()
	<-waiting
	bdone := make(chan error, 1)
	go func() { batch := NewBatch(1); batch.Append(Insert(2, 2)); bdone <- b.Apply(batch) }()
	select {
	case err := <-bdone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated namespace blocked")
	}
	if pinned.shards[0].times[0] != 1 {
		t.Fatal("pinned extent changed")
	}
	a.readers.Release(token)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.crash = nil
	path := a.path
	if err := s.CloseNamespace(1); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 1024), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := s.OpenNamespace(1); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	apply(t, b, Insert(3, 3))
	if b.Len() != 3 {
		t.Fatal("corruption isolation")
	}
}
