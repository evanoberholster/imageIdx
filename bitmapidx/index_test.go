package bitmapidx

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestBatchAtomicVisibility(t *testing.T) {
	index, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 9}, IndexID: 11}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	ids := make([]uint64, 1_000)
	for n := range ids {
		ids[n] = uint64(n + 1)
	}

	stop := make(chan struct{})
	errCh := make(chan int, 1)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				count := len(index.AppendTo(nil))
				if count != 0 && count != 1_000 {
					select {
					case errCh <- count:
					default:
					}
					return
				}
			}
		}()
	}
	if err = index.Apply(Mutation{Add: ids}); err != nil {
		t.Fatal(err)
	}
	close(stop)
	readers.Wait()
	select {
	case count := <-errCh:
		t.Fatalf("reader observed partial batch of %d", count)
	default:
	}
	if got := len(index.AppendTo(nil)); got != 1_000 {
		t.Fatalf("got %d IDs", got)
	}
}

func TestCancellationAndPrecedence(t *testing.T) {
	index, err := New(Spec{Key: Key{Type: IndexTypeAlbum, ValueID: 3}, IndexID: 4, IDs: []ImageID{1, 2, 3}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err = index.Apply(Mutation{Add: []ImageID{4, 2}, Remove: []ImageID{2, 3, 4}}); err != nil {
		t.Fatal(err)
	}
	if got := index.AppendTo(nil); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("unexpected canonical state: %v", got)
	}
	view := index.view.Load()
	if view.active.len() != 2 || !view.active.removals.Contains(2) || !view.active.removals.Contains(3) {
		t.Fatalf("unexpected delta: %#v", view.active)
	}
}

func TestAndOrWithOperandAwareRemovals(t *testing.T) {
	config := Config{}
	a, _ := New(Spec{Key: Key{Type: IndexTypeLocation, ValueID: 1}, IndexID: 1, IDs: []ImageID{1, 2, 3}}, config)
	b, _ := New(Spec{Key: Key{Type: IndexTypeLocation, ValueID: 2}, IndexID: 2, IDs: []ImageID{3, 4}}, config)
	defer a.Close()
	defer b.Close()
	_ = a.Apply(Mutation{Add: []ImageID{5}, Remove: []ImageID{3}})
	_ = b.Apply(Mutation{Add: []ImageID{2}, Remove: []ImageID{4}})
	if got := Intersect(a, b).ToArray(); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("AND = %v", got)
	}
	if got := Union(a, b).ToArray(); !slices.Equal(got, []uint64{1, 2, 3, 5}) {
		t.Fatalf("OR = %v", got)
	}
}

func TestBoundedLayersAndCompaction(t *testing.T) {
	index, err := New(Spec{Key: Key{Type: IndexTypePerson, ValueID: 7}, IndexID: 7}, Config{
		ActiveTarget: 4, HardDeltaLimit: 8, CompactDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for start := uint64(0); start < 200; start += 3 {
		if err = index.Apply(Mutation{Add: []ImageID{start + 1, start + 2, start + 3}}); err != nil {
			t.Fatal(err)
		}
		view := index.view.Load()
		if view.frozen != nil && view.active != nil && view.active.len() > 8 {
			t.Fatalf("unbounded active delta: %d", view.active.len())
		}
	}
	if err = index.Compact(); err != nil {
		t.Fatal(err)
	}
	view := index.view.Load()
	if view.frozen != nil || view.active != nil || view.cardinality != 201 {
		t.Fatalf("unexpected compacted view: %#v", view)
	}
	_ = index.Close()
}

func TestConcurrentReadsDuringCompaction(t *testing.T) {
	const stableID = ImageID(1_000_000)
	index, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: []ImageID{stableID}}, Config{
		ActiveTarget: 8, HardDeltaLimit: 64, CompactDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	stop := make(chan struct{})
	errCh := make(chan string, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if !index.Contains(stableID) {
					select {
					case errCh <- "stable base member lost during rotation/compaction":
					default:
					}
					return
				}
				ids := index.AppendTo(nil)
				for i := 1; i < len(ids); i++ {
					if ids[i] <= ids[i-1] {
						select {
						case errCh <- "AppendTo observed out-of-order or duplicated IDs":
						default:
						}
						return
					}
				}
			}
		}()
	}

	// Sustained small writes force repeated Active->Frozen rotations and
	// background Base compaction while readers run.
	for n := ImageID(0); n < 2_000; n++ {
		if err = index.Apply(Mutation{Add: []ImageID{n}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = index.Apply(Mutation{Remove: []ImageID{10, 20, 30}}); err != nil {
		t.Fatal(err)
	}
	if err = index.WaitIdle(t.Context()); err != nil {
		t.Fatal(err)
	}

	close(stop)
	readers.Wait()
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
	if !index.Contains(stableID) {
		t.Fatal("stable base member lost after compaction")
	}
}

func TestPlannerUsesBaseCardinality(t *testing.T) {
	a, _ := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: []ImageID{1, 2, 3}}, Config{})
	b, _ := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 2}, IndexID: 2, IDs: []ImageID{1}}, Config{})
	defer a.Close()
	defer b.Close()
	_ = b.Apply(Mutation{Add: []ImageID{2, 3, 4, 5, 6}})
	planned := planIntersection([]*Index{a, b})
	if planned[0] != b {
		t.Fatal("planner inspected delta cardinality")
	}
}

func TestStandaloneIndexesOwnReaderTrackers(t *testing.T) {
	a, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 2}, IndexID: 2}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if a.readers == nil || b.readers == nil {
		t.Fatal("standalone index has no reader tracker")
	}
	if a.readers == b.readers {
		t.Fatal("standalone indexes unexpectedly share reader tracking")
	}
}
