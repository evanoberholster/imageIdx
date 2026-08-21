package bitmapidx

import (
	"slices"
	"sync"
	"testing"
	"time"
)

func TestBatchAtomicVisibility(t *testing.T) {
	index, err := New(BitmapKey{Type: IndexTypeTag, ValueID: 9}, 11, nil, Config{})
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
				count := len(index.Search(nil))
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
	if err = index.Add(ids); err != nil {
		t.Fatal(err)
	}
	close(stop)
	readers.Wait()
	select {
	case count := <-errCh:
		t.Fatalf("reader observed partial batch of %d", count)
	default:
	}
	if got := len(index.Search(nil)); got != 1_000 {
		t.Fatalf("got %d IDs", got)
	}
}

func TestCancellationAndPrecedence(t *testing.T) {
	index, err := New(BitmapKey{Type: IndexTypeAlbum, ValueID: 3}, 4, []uint64{1, 2, 3}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err = index.Write(BitmapMutation{Additions: []uint64{4, 2}, Removals: []uint64{2, 3, 4}}); err != nil {
		t.Fatal(err)
	}
	if got := index.Search(nil); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("unexpected canonical state: %v", got)
	}
	view := index.LoadView()
	if view.Active.Len() != 2 || !view.Active.Removals.Contains(2) || !view.Active.Removals.Contains(3) {
		t.Fatalf("unexpected delta: %#v", view.Active)
	}
}

func TestAndOrWithOperandAwareRemovals(t *testing.T) {
	epoch := NewEpoch()
	config := Config{Epoch: epoch}
	a, _ := New(BitmapKey{Type: IndexTypeLocation, ValueID: 1}, 1, []uint64{1, 2, 3}, config)
	b, _ := New(BitmapKey{Type: IndexTypeLocation, ValueID: 2}, 2, []uint64{3, 4}, config)
	defer a.Close()
	defer b.Close()
	_ = a.Write(BitmapMutation{Additions: []uint64{5}, Removals: []uint64{3}})
	_ = b.Write(BitmapMutation{Additions: []uint64{2}, Removals: []uint64{4}})
	if got := And(a, b).ToArray(); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("AND = %v", got)
	}
	if got := Or(a, b).ToArray(); !slices.Equal(got, []uint64{1, 2, 3, 5}) {
		t.Fatalf("OR = %v", got)
	}
}

func TestBoundedLayersAndCompaction(t *testing.T) {
	index, err := New(BitmapKey{Type: IndexTypePerson, ValueID: 7}, 7, nil, Config{
		ActiveTarget: 4, HardDeltaLimit: 8, CompactDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for start := uint64(0); start < 200; start += 3 {
		if err = index.Add([]uint64{start, start + 1, start + 2}); err != nil {
			t.Fatal(err)
		}
		view := index.LoadView()
		if view.Frozen != nil && view.Active != nil && view.Active.Len() > 8 {
			t.Fatalf("unbounded active delta: %d", view.Active.Len())
		}
	}
	if err = index.Compact(); err != nil {
		t.Fatal(err)
	}
	view := index.LoadView()
	if view.Frozen != nil || view.Active != nil || view.Cardinality != 201 {
		t.Fatalf("unexpected compacted view: %#v", view)
	}
	_ = index.Close()
}

func TestPlannerUsesBaseCardinality(t *testing.T) {
	epoch := NewEpoch()
	a, _ := New(BitmapKey{Type: IndexTypeTag, ValueID: 1}, 1, []uint64{1, 2, 3}, Config{Epoch: epoch})
	b, _ := New(BitmapKey{Type: IndexTypeTag, ValueID: 2}, 2, []uint64{1}, Config{Epoch: epoch})
	defer a.Close()
	defer b.Close()
	_ = b.Add([]uint64{2, 3, 4, 5, 6})
	planned := PlanAND([]*BitmapIndex{a, b})
	if planned[0] != b {
		t.Fatal("planner inspected delta cardinality")
	}
}
