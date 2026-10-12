package idalloc

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/common"
)

type memoryStore struct {
	mu      sync.Mutex
	high    [common.EntityKindCount + 1]uint64
	writes  int
	fail    bool
	entered chan struct{}
	release chan struct{}
}

func (s *memoryStore) LoadReservedThrough(k common.EntityKind) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.high[k], nil
}
func (s *memoryStore) ReserveThrough(k common.EntityKind, high uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entered != nil {
		close(s.entered)
		<-s.release
		s.entered = nil
	}
	if s.fail {
		return errors.New("injected write failure")
	}
	if high < s.high[k] {
		return errors.New("watermark lowered")
	}
	s.high[k] = high
	s.writes++
	return nil
}

func TestBoundaryReopenAndFailure(t *testing.T) {
	s := &memoryStore{}
	a, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= BlockSize; i++ {
		id, err := a.Next(common.EntityImage)
		if err != nil || uint64(id) != i {
			t.Fatalf("%d: %d, %v", i, id, err)
		}
	}
	if s.writes != 1 || s.high[common.EntityImage] != 1000 {
		t.Fatalf("high=%d writes=%d", s.high[common.EntityImage], s.writes)
	}
	s.fail = true
	if id, err := a.Next(common.EntityImage); id != 0 || err == nil {
		t.Fatalf("failed reservation: %d, %v", id, err)
	}
	s.fail = false
	if id, err := a.Next(common.EntityImage); id != 1001 || err != nil {
		t.Fatalf("retry: %d, %v", id, err)
	}
	if id, err := a.Next(common.EntityFace); id != 1 || err != nil {
		t.Fatalf("face: %d, %v", id, err)
	}
	_ = a.Close()
	if _, err := a.Next(common.EntityImage); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	b, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := b.Next(common.EntityImage); id != 2001 || err != nil {
		t.Fatalf("reopen image: %d, %v", id, err)
	}
	if id, err := b.Next(common.EntityFace); id != 1001 || err != nil {
		t.Fatalf("reopen face: %d, %v", id, err)
	}
	if _, err := b.Next(common.EntityInvalid); !errors.Is(err, ErrKind) {
		t.Fatal(err)
	}
}

func TestOverflow(t *testing.T) {
	s := &memoryStore{}
	s.high[common.EntityImage] = math.MaxUint64 - 2
	a, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(math.MaxUint64 - 1); i <= math.MaxUint64; i++ {
		id, err := a.Next(common.EntityImage)
		if uint64(id) != i || err != nil {
			t.Fatalf("%d, %v", id, err)
		}
		if i == math.MaxUint64 {
			break
		}
	}
	if id, err := a.Next(common.EntityImage); id != 0 || !errors.Is(err, ErrExhausted) {
		t.Fatalf("%d, %v", id, err)
	}
}

func TestSkipThroughDurableBeforePublishing(t *testing.T) {
	s := &memoryStore{}
	a, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	s.fail = true
	if err := a.SkipThrough(common.EntityImage, 5400); err == nil {
		t.Fatal("accepted external ID without durable reservation")
	}
	s.fail = false
	if id, err := a.Next(common.EntityImage); id != 1 || err != nil {
		t.Fatalf("failed reconciliation consumed ID: %d, %v", id, err)
	}
	if err := a.SkipThrough(common.EntityImage, 5400); err != nil {
		t.Fatal(err)
	}
	if id, err := a.Next(common.EntityImage); id != 5401 || err != nil {
		t.Fatalf("external ID reused: %d, %v", id, err)
	}
	_ = a.Close()
	b, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := b.Next(common.EntityImage); id != 6401 || err != nil {
		t.Fatalf("reopen: %d, %v", id, err)
	}
}

func TestConcurrentReservations(t *testing.T) {
	s := &memoryStore{}
	a, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	const workers, each = 64, 180
	ids := make([]uint64, workers*each)
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(n)))
			for j := 0; j < each; j++ {
				if r.Intn(12) == 0 {
					time.Sleep(time.Microsecond)
				}
				id, err := a.Next(common.EntityImage)
				if err != nil {
					t.Error(err)
					return
				}
				ids[n*each+j] = uint64(id)
			}
		}(n)
	}
	wg.Wait()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		if id != uint64(i+1) {
			t.Fatalf("at %d: %d", i, id)
		}
	}
	if s.writes != 12 {
		t.Fatalf("writes=%d", s.writes)
	}
}

func TestCloseWaitsForReservation(t *testing.T) {
	s := &memoryStore{entered: make(chan struct{}), release: make(chan struct{})}
	a, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	issued := make(chan error, 1)
	go func() { _, err := a.Next(common.EntityImage); issued <- err }()
	<-s.entered
	closed := make(chan struct{})
	go func() { _ = a.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("closed during reservation")
	case <-time.After(time.Millisecond * 10):
	}
	close(s.release)
	if err := <-issued; err != nil {
		t.Fatal(err)
	}
	<-closed
}

func TestSteadyStateZeroAllocations(t *testing.T) {
	s := &memoryStore{}
	a, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Uint64
	if allocs := testing.AllocsPerRun(100, func() {
		id, err := a.Next(common.EntityImage)
		if err != nil {
			panic(err)
		}
		count.Store(uint64(id))
	}); allocs != 0 {
		t.Fatalf("allocs=%g", allocs)
	}
	if count.Load() == 0 {
		t.Fatal("no ID")
	}
}
