package storage

import (
	"sync"
	"testing"
)

func TestReaderTrackerWaitsForPriorReaders(t *testing.T) {
	var readers ReaderTracker
	slot := readers.Acquire()
	done := make(chan struct{})
	go func() {
		readers.WaitForPriorReaders()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("wait returned while the old reader was active")
	default:
	}
	readers.Release(slot)
	<-done
}

func TestReaderTrackerConcurrentEntry(t *testing.T) {
	var readers ReaderTracker
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 1_000 {
				slot := readers.Acquire()
				readers.Release(slot)
			}
		}()
	}
	workers.Wait()
	readers.WaitForPriorReaders()
}
