package simhash

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentReadersDuringUpdate runs lock-free readers against in-place
// point updates. Run with -race.
func TestConcurrentReadersDuringUpdate(t *testing.T) {
	st := openStore(t, testConfig())
	const n = 4096
	records := randRecords(31, n)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	updates := randRecords(32, n)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = st.SearchPHash(nil, records[0].PHash, 8)
				_ = st.SearchPDQ(nil, &records[0].PDQ, 16)
				_, _ = st.Get(1)
			}
		}()
	}
	for i := 0; i < n; i++ {
		if err := st.Upsert(uint64(i+1), updates[i]); err != nil {
			t.Fatalf("Upsert(%d): %v", i+1, err)
		}
	}
	close(stop)
	wg.Wait()
	for i := 0; i < n; i++ {
		got, ok := st.Get(uint64(i + 1))
		if !ok || got != updates[i] {
			t.Fatalf("Get(%d) = %+v, want %+v", i+1, got, updates[i])
		}
	}
}

// TestNoTornPDQ ties the pHash to the PDQ value and asserts a reader never
// observes a mixed tuple across concurrent upserts.
func TestNoTornPDQ(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(33, 1)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	var a, b Record
	a.PHash = 0xAAAAAAAAAAAAAAAA
	b.PHash = 0x5555555555555555
	for i := range a.PDQ {
		a.PDQ[i] = 0xAA
		b.PDQ[i] = 0x55
	}
	a.Quality = 0xAA
	b.Quality = 0x55
	// Seed a valid state before readers start so every observed tuple is a or b.
	if err := st.Upsert(1, a); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var torn int64
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec, ok := st.Get(1)
				if !ok {
					continue
				}
				switch rec.PHash {
				case a.PHash:
					if rec.PDQ != a.PDQ || rec.Quality != a.Quality {
						atomic.AddInt64(&torn, 1)
					}
				case b.PHash:
					if rec.PDQ != b.PDQ || rec.Quality != b.Quality {
						atomic.AddInt64(&torn, 1)
					}
				default:
					atomic.AddInt64(&torn, 1)
				}
			}
		}()
	}
	for i := 0; i < 200_000; i++ {
		if i&1 == 0 {
			if err := st.Upsert(1, a); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
		} else if err := st.Upsert(1, b); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if torn != 0 {
		t.Fatalf("observed %d torn reads", torn)
	}
}

func TestSequenceOddEven(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(34, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Upsert(1, randRecords(35, 1)[0]); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	v := st.view.Load()
	sh, pi := v.shardForHashPage(0)
	seq := atomic.LoadUint64(sh.seqPtr(pi, 0))
	if seq == 0 {
		t.Fatal("sequence not advanced by an update")
	}
	if seq&1 != 0 {
		t.Fatalf("sequence %d is odd after a completed update", seq)
	}
}
