package bitmapidx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestFieldFrozenSurvivesSecondRotation(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{ActiveTarget: 2, HardDeltaLimit: 8, CompactDelay: 20 * time.Millisecond})
	for batch := 0; batch < 2; batch++ {
		var entries []Entry
		for n := 1; n <= 3; n++ {
			entries = append(entries, Entry{ImageID: uint64(batch*3 + n), Value: StringValue("a")})
		}
		if err := index.Add(entries...); err != nil {
			t.Fatal(err)
		}
	}
	if got := appendIDs(t, index, StringValue("a")); !slices.Equal(got, []ImageID{1, 2, 3, 4, 5, 6}) {
		t.Fatal(got)
	}
}

func TestDictionaryConcurrentFirstUses(t *testing.T) {
	index := mustField(t, enumField(IndexTypeTag), FieldConfig{})
	if err := index.Add(Entry{ImageID: 1, Value: StringValue("seed")}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					index.Contains(1, StringValue("seed"))
				}
			}
		}()
	}
	for n := 0; n < 200; n++ {
		if err := index.Add(Entry{ImageID: uint64(n + 2), Value: StringValue(fmt.Sprintf("new-%d", n))}); err != nil {
			t.Error(err)
		}
	}
	close(done)
	wg.Wait()
}

func regressionStore(t *testing.T) (string, *Store, Key) {
	t.Helper()
	dir := t.TempDir()
	key := Key{Type: IndexTypeTag, ValueID: 1}
	s, err := NewPersistent(dir, []Spec{{Key: key, IndexID: 1, IDs: []ImageID{1}}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return dir, s, key
}

func TestPersistentChildUsesWAL(t *testing.T) {
	dir, s, key := regressionStore(t)
	idx, _ := s.Lookup(key)
	if err := idx.Apply(Mutation{Add: []ImageID{2}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); !errors.Is(err, ErrOwnedIndex) {
		t.Fatal(err)
	}
	s.Close()
	s, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	idx, _ = s.Lookup(key)
	if !idx.Contains(2) {
		t.Fatal("child write lost")
	}
}

func TestStoreOwnershipAndCreateProtection(t *testing.T) {
	dir, s, _ := regressionStore(t)
	if other, err := OpenPersistent(dir, Config{}, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	s.Close()
	if other, err := NewPersistent(dir, nil, Config{}, DurabilityOS); !errors.Is(err, ErrStoreExists) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	s, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestPredicateTornTailRepairAndAppend(t *testing.T) {
	for _, tail := range [][]byte{{1, 2, 3}, make([]byte, bitmapWALHeaderSize+3)} {
		dir, s, key := regressionStore(t)
		if err := s.Add(key, []ImageID{2}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		path := filepath.Join(dir, bitmapWALFilename)
		if len(tail) > 3 {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			copy(tail, data[:len(tail)])
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(tail); err != nil {
			t.Fatal(err)
		}
		f.Close()
		s, err = OpenPersistent(dir, Config{}, DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Add(key, []ImageID{3}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s, err = OpenPersistent(dir, Config{}, DurabilityOS)
		if err != nil {
			t.Fatal(err)
		}
		idx, _ := s.Lookup(key)
		if !idx.Contains(3) {
			t.Fatal("lost post-recovery write")
		}
		s.Close()
	}
}

func TestPredicateResultsSurviveClose(t *testing.T) {
	dir, s, key := regressionStore(t)
	s.Close()
	s, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := s.Lookup(key)
	a, o := And(idx), Or(nil, idx)
	s.Close()
	if !a.Contains(1) || !o.Contains(1) {
		t.Fatal("result lost contents")
	}
}

func TestPredicateConcurrentCheckpointVacuum(t *testing.T) {
	dir, s, _ := regressionStore(t)
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(vacuum bool) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				var err error
				if vacuum {
					err = s.Vacuum()
				} else {
					err = s.Checkpoint()
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(n == 0)
	}
	wg.Wait()
	s.Close()
	s, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestPredicateWALFailureStopsCheckpoint(t *testing.T) {
	_, s, key := regressionStore(t)
	s.wal.Close()
	if err := s.Add(key, []ImageID{2}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatal(err)
	}
	idx, _ := s.Lookup(key)
	if idx.Contains(2) {
		t.Fatal("failed write published")
	}
}
