package commit

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestQueueGroupsBursts(t *testing.T) {
	var groups atomic.Int64
	var committed atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	q := New[int](8, func(group []int) {
		groups.Add(1)
		committed.Add(int64(len(group)))
		once.Do(func() {
			close(entered)
			<-release
		})
	})
	q.Submit(1)
	<-entered // the loop is inside the first commit
	for n := 0; n < 4; n++ {
		q.Submit(2) // queued behind the held commit
	}
	close(release)
	q.Stop()
	q.Wait()
	if got := committed.Load(); got != 5 {
		t.Fatalf("committed=%d want 5", got)
	}
	if got := groups.Load(); got != 2 {
		t.Fatalf("groups=%d want 2 (one held, one coalesced burst)", got)
	}
}

func TestQueueSubmitAfterStop(t *testing.T) {
	var calls atomic.Int64
	q := New[int](1, func(group []int) { calls.Add(int64(len(group))) })
	q.Submit(1)
	q.Stop()
	q.Wait()
	if q.Submit(2) {
		t.Fatal("Submit after Stop reported success")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls=%d want 1 (the post-stop request must be dropped)", got)
	}
}

func TestQueueStopDrains(t *testing.T) {
	var mu sync.Mutex
	committed := make(map[int]bool)
	block := make(chan struct{})
	var first sync.Once
	q := New[int](64, func(group []int) {
		mu.Lock()
		for _, v := range group {
			committed[v] = true
		}
		mu.Unlock()
		first.Do(func() { <-block })
	})
	q.Submit(0) // occupy the loop so the rest queue behind it
	for n := 1; n <= 50; n++ {
		q.Submit(n)
	}
	close(block)
	q.Stop()
	q.Wait()
	mu.Lock()
	defer mu.Unlock()
	for n := 0; n <= 50; n++ {
		if !committed[n] {
			t.Fatalf("request %d was not drained", n)
		}
	}
}

// TestQueueStopRaceKeepsAcceptedRequests checks the core contract: every
// Submit that returns true is committed exactly once, even when Stop
// races concurrent submitters.
func TestQueueStopRaceKeepsAcceptedRequests(t *testing.T) {
	var mu sync.Mutex
	committed := make(map[uint64]int)
	q := New[uint64](4, func(group []uint64) {
		mu.Lock()
		defer mu.Unlock()
		for _, v := range group {
			committed[v]++
		}
	})
	const submitters = 8
	const perSubmitter = 64
	var accepted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for s := 0; s < submitters; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			<-start
			for n := 0; n < perSubmitter; n++ {
				id := uint64(s*perSubmitter + n)
				if q.Submit(id) {
					accepted.Add(1)
				}
			}
		}(s)
	}
	close(start)
	q.Stop()
	wg.Wait()
	q.Wait()
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, count := range committed {
		if count != 1 {
			t.Fatalf("request committed %d times", count)
		}
		total++
	}
	if int64(total) != accepted.Load() {
		t.Fatalf("committed=%d accepted=%d", total, accepted.Load())
	}
}

func TestQueueStartsLazily(t *testing.T) {
	q := New[int](1, func([]int) {})
	if q.Started() {
		t.Fatal("queue started before first Submit")
	}
	q.Submit(1)
	q.Stop()
	q.Wait()
	if !q.Started() {
		t.Fatal("queue did not start on Submit")
	}
	other := New[int](1, func([]int) {})
	other.Stop()
	other.Wait() // never started: Wait returns immediately
}

func TestQueueKeepsEveryRequest(t *testing.T) {
	seen := map[int]bool{}
	var mu sync.Mutex
	q := New[int](8, func(group []int) {
		mu.Lock()
		defer mu.Unlock()
		for _, v := range group {
			seen[v] = true
		}
	})
	for n := 0; n < 4; n++ {
		q.Submit(n)
	}
	q.Stop()
	q.Wait()
	for n := 0; n < 4; n++ {
		if !seen[n] {
			t.Fatalf("request %d missing", n)
		}
	}
}
