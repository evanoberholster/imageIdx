package timeidx

import (
	"errors"
	"runtime"
	"sync"
	"testing"
)

func TestResolvedHandlesAllMethodsDuringShutdown(t *testing.T) {
	s, rs, err := InitializeStore(t.TempDir(), []NamespaceInit{{Spec: Spec{Namespace: 1, Entries: []Entry{{Timestamp: 1, ImageID: 1}}}, Create: true}, {Spec: Spec{Namespace: 2, Entries: []Entry{{Timestamp: 1, ImageID: 1}}}, Create: true}}, Config{}, DurabilitySync)
	if err != nil || rs[0].Err != nil || rs[1].Err != nil {
		t.Fatal(err, rs)
	}
	var wg sync.WaitGroup
	ready := make(chan struct{}, 2)
	stop := make(chan struct{})
	for _, ns := range []uint16{1, 2} {
		i := s.indexes[int(ns)-1].index
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]uint64, 0, 4)
			ready <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
				}
				dst = i.Search(dst[:0], 0, 2)
				dst = i.SearchExact(dst[:0], 0, 2, func(uint64) (uint64, bool) { return 1, true })
				i.EstimateRange(0, 2)
				i.Metadata()
				i.Generation()
				i.Len()
				i.CommitStats()
				i.OnDiskBytes()
				i.SourceGeneration()
				i.CheckSourceGeneration(1)
				s.Namespace(i.Namespace())
				s.NamespaceError(i.Namespace())
			}
		}()
	}
	<-ready
	<-ready
	var closers sync.WaitGroup
	for n := 0; n < 4; n++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	closers.Wait()
	close(stop)
	wg.Wait()
	for _, e := range s.indexes {
		i := e.index
		if len(i.Search(nil, 0, 2)) != 0 || len(i.SearchExact(nil, 0, 2, nil)) != 0 || i.EstimateRange(0, 2) != 0 {
			t.Fatal("closed mmap read")
		}
		if err := i.Apply(NewBatch(0)); err != ErrClosed {
			t.Fatal(err)
		}
		if err := i.ApplyWithGeneration(NewBatch(0), 1); err != ErrClosed {
			t.Fatal(err)
		}
		i.Metadata()
		i.Generation()
		i.Len()
		i.CommitStats()
		i.OnDiskBytes()
	}
}

func TestSearchExactPinsAcrossStoreClose(t *testing.T) {
	s, _, err := InitializeStore(t.TempDir(), []NamespaceInit{{Spec: Spec{Namespace: 1, Entries: []Entry{{Timestamp: 1, ImageID: 1}}}, Create: true}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.Namespace(1)
	entered, release := make(chan struct{}), make(chan struct{})
	searched := make(chan []uint64, 1)
	go func() {
		searched <- i.SearchExact(nil, 0, 2, func(uint64) (uint64, bool) { close(entered); <-release; return 1, true })
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	for !s.closed.Load() {
		runtime.Gosched()
	}
	select {
	case <-closed:
		t.Fatal("unmapped pinned search")
	default:
	}
	close(release)
	if got := <-searched; len(got) != 1 || got[0] != 1 {
		t.Fatal(got)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestGrowthApplyVersusStoreClose(t *testing.T) {
	s, _, err := InitializeStore(t.TempDir(), []NamespaceInit{{Spec: Spec{Namespace: 1}, Create: true}}, Config{MaxFileBytes: 4096, MaxShardBytes: 512}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.Namespace(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	i.crash = func(stage string) {
		if stage == "grow" {
			once.Do(func() { close(entered); <-release })
		}
	}
	b := NewBatch(1000)
	for n := uint64(0); n < 1000; n++ {
		b.Append(Insert(n, n))
	}
	done := make(chan error, 1)
	go func() { done <- i.Apply(b) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	for !s.closed.Load() {
		runtime.Gosched()
	}
	if _, ok := s.Namespace(1); ok {
		t.Fatal("shutdown resolved handle")
	}
	close(release)
	if err := <-done; err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestSparseMutationFilterCollisions(t *testing.T) {
	const a = uint64(1)
	var collision uint64
	for n := uint64(5000); ; n++ {
		if mutationBit(n) == mutationBit(a) {
			collision = n
			break
		}
	}
	i := mustNewIndex(t, []Entry{{Timestamp: 1, ImageID: a}, {Timestamp: 2, ImageID: collision}, {Timestamp: 3, ImageID: ^uint64(0)}}, 0, Config{MaxShardBytes: 16})
	apply(t, i, Delete(a, 1), Delete(^uint64(0), 3))
	if got := i.Search(nil, 0, 4); len(got) != 1 || got[0] != collision {
		t.Fatal("filter treated collision as membership", got)
	}
}
