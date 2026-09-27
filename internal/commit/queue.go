// Package commit provides the shared single-owner group-commit pipeline
// used by the index packages.
//
// A Queue serializes commit work onto one goroutine. Callers Submit one
// request at a time; the loop gathers each request plus everything already
// queued behind it into one group and calls Commit once per group. The
// group is the natural unit of both write-lock hold and durability sync:
// coalescing concurrent writers means a group of N requests pays one lock
// acquisition and one fsync instead of N.
//
// The loop starts lazily on the first Submit, so read-only indexes never
// spawn a goroutine. Submit and Stop are mutually exclusive: once Submit
// returns true the request is guaranteed to be committed exactly once,
// even when Stop races it. Stop drains every queued request through
// Commit before the loop exits, so Stop followed by Wait is the shutdown
// handshake.
package commit

import (
	"sync"
	"sync/atomic"
)

// defaultQueueCapacity bounds queued requests. Submit blocks when the
// queue is full, so bursty writers exert backpressure instead of growing
// memory without bound.
const defaultQueueCapacity = 64

// Queue is a single-owner group-commit pipeline over requests of type T.
// Commit runs on the queue's goroutine and receives one group at a time;
// it must not retain the slice after returning.
type Queue[T any] struct {
	// mu makes Submit and Stop mutually exclusive so a successful Submit
	// is always drained: Stop cannot begin until every in-flight Submit
	// has enqueued, and no Submit succeeds after Stop.
	mu      sync.RWMutex
	ch      chan T
	stop    chan struct{}
	started atomic.Bool
	stopped atomic.Bool
	start   sync.Once
	workers sync.WaitGroup
	commit  func([]T)
	group   []T
}

// New returns a Queue that calls commit for each group of requests.
// capacity <= 0 selects the default queue depth.
func New[T any](capacity int, commit func([]T)) *Queue[T] {
	if capacity <= 0 {
		capacity = defaultQueueCapacity
	}
	return &Queue[T]{
		ch:     make(chan T, capacity),
		stop:   make(chan struct{}),
		commit: commit,
	}
}

// Start launches the commit goroutine on first use. Submit calls it
// automatically; it is exported so callers that need the guarantee can
// start the loop before their first request.
func (q *Queue[T]) Start() {
	q.start.Do(func() {
		q.started.Store(true)
		q.workers.Add(1)
		go q.loop()
	})
}

// Started reports whether the commit loop has been launched.
func (q *Queue[T]) Started() bool { return q.started.Load() }

// Submit enqueues req, blocking while the queue is full. It reports false
// when the queue has stopped, in which case req was not enqueued. A true
// return guarantees req is committed exactly once: Stop waits for
// in-flight submissions and drains everything queued before the loop
// exits.
func (q *Queue[T]) Submit(req T) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.stopped.Load() {
		return false
	}
	q.Start()
	// Stop cannot close q.stop while this read lock is held, and the loop
	// only exits after Stop, so this send always completes.
	q.ch <- req
	return true
}

// Stop signals the loop to drain remaining requests and exit. It is
// idempotent and does not wait; call Wait for the shutdown guarantee.
func (q *Queue[T]) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped.Swap(true) {
		return
	}
	close(q.stop)
}

// Wait blocks until the commit loop has drained and exited. It returns
// immediately when the loop was never started.
func (q *Queue[T]) Wait() { q.workers.Wait() }

func (q *Queue[T]) loop() {
	defer q.workers.Done()
	for {
		select {
		case req := <-q.ch:
			q.commit(q.collect(req))
		case <-q.stop:
			q.drain()
			return
		}
	}
}

// collect gathers first plus every request already queued behind it. The
// slice is loop-owned and reused across groups; Commit must not retain it.
func (q *Queue[T]) collect(first T) []T {
	q.group = append(q.group[:0], first)
	for {
		select {
		case req := <-q.ch:
			q.group = append(q.group, req)
		default:
			return q.group
		}
	}
}

// drain commits everything still queued after stop so accepted requests
// are not lost.
func (q *Queue[T]) drain() {
	for {
		select {
		case req := <-q.ch:
			q.commit(q.collect(req))
		default:
			return
		}
	}
}
