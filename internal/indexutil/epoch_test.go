package indexutil

import (
	"sync"
	"testing"
)

func TestReaderEpochWaitsForPriorReaders(t *testing.T) {
	var epoch ReaderEpoch
	slot := epoch.Enter()
	done := make(chan struct{})
	go func() {
		epoch.Synchronize()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("synchronize returned while the old reader was active")
	default:
	}
	epoch.Exit(slot)
	<-done
}

func TestReaderEpochConcurrentEntry(t *testing.T) {
	var epoch ReaderEpoch
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 1_000 {
				slot := epoch.Enter()
				epoch.Exit(slot)
			}
		}()
	}
	workers.Wait()
	epoch.Synchronize()
}
