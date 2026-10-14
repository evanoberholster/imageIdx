package schema

import (
	"errors"
	"math/rand"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestConcurrentMixedAllocationAndMinimum(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		for _, workers := range []int{1, 2, 4, 8, 16, 32, 64} {
			t.Run(string(rune('A'+workers)), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "schema.dat")
				s, err := Open(path, singleOptions(b, 123, "mixed", 19))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				const rounds = 60
				var wg sync.WaitGroup
				var mu sync.Mutex
				var ranges [][2]uint64
				for w := 0; w < workers; w++ {
					wg.Add(1)
					go func(w int) {
						defer wg.Done()
						rng := rand.New(rand.NewSource(int64(w + 1)))
						for i := 0; i < rounds; i++ {
							if rng.Intn(10) == 0 {
								time.Sleep(time.Microsecond)
							}
							if rng.Intn(15) == 0 {
								// Multiple concurrent floor raises; no allocation CAS may
								// duplicate a range, regardless of their completion order.
								if err := s.EnsureMinimum(EntityImage, uint64(w*rounds+i+10000)); err != nil {
									t.Error(err)
									return
								}
								continue
							}
							count := uint64(rng.Intn(11) + 1)
							start, err := s.NextIDs(EntityImage, count)
							if err != nil {
								t.Error(err)
								return
							}
							mu.Lock()
							ranges = append(ranges, [2]uint64{start, start + count})
							mu.Unlock()
						}
					}(w)
				}
				wg.Wait()
				sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
				for i := 1; i < len(ranges); i++ {
					if ranges[i][0] < ranges[i-1][1] {
						t.Fatalf("overlap %v and %v", ranges[i-1], ranges[i])
					}
				}
			})
		}
	})
}

func TestCloseAndPersistenceFailure(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		path := filepath.Join(t.TempDir(), "schema.dat")
		opts := singleOptions(b, 33, "x", 4)
		s, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		s.fault = func(stage string) error {
			if stage == "after metadata write" {
				close(entered)
				<-release
				return errors.New("injected")
			}
			return nil
		}
		result := make(chan error, 1)
		go func() { _, err := s.NextID(EntityImage); result <- err }()
		<-entered
		closed := make(chan error, 1)
		go func() { closed <- s.Close() }()
		select {
		case <-closed:
			t.Fatal("closed before reservation drained")
		case <-time.After(5 * time.Millisecond):
		}
		close(release)
		if err := <-result; err == nil {
			t.Fatal("issued ID from failed reservation")
		}
		if err := <-closed; !errors.Is(err, ErrCorrupt) {
			t.Fatalf("failed close: %v", err)
		}
		if _, err := s.NextID(EntityImage); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		// The inactive slot may be valid even though sync was not attempted.
		// Either skipping its reservation or failing closed is safe; returning
		// an ID from the failed block before durable evidence would not be.
		s, err = Open(path, opts)
		if err == nil {
			if id, err := s.NextID(EntityImage); err != nil || (id != 1 && id <= 4) {
				t.Fatalf("recovery: %d %v", id, err)
			}
			_ = s.Close()
		}
	})
}

func TestZeroAllocationFastPath(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		s, err := Open(filepath.Join(t.TempDir(), "schema.dat"), singleOptions(b, 12, "fast", 100000))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.NextID(EntityImage); err != nil {
			t.Fatal(err)
		}
		if n := testing.AllocsPerRun(1000, func() {
			if _, err := s.NextID(EntityImage); err != nil {
				panic(err)
			}
		}); n != 0 {
			t.Fatalf("NextID allocations: %g", n)
		}
		if n := testing.AllocsPerRun(1000, func() {
			if _, err := s.NextIDs(EntityImage, 8); err != nil {
				panic(err)
			}
		}); n != 0 {
			t.Fatalf("NextIDs allocations: %g", n)
		}
	})
}

func TestEnsureMinimumAndCloseRaces(t *testing.T) {
	forBackends(t, func(t *testing.T, b Backend) {
		s, err := Open(filepath.Join(t.TempDir(), "schema.dat"), singleOptions(b, 88, "races", 13))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for w := 0; w < 32; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 150; i++ {
					var err error
					switch w % 3 {
					case 0:
						_, err = s.NextID(EntityImage)
					case 1:
						_, err = s.NextIDs(EntityImage, 23)
					default:
						err = s.EnsureMinimum(EntityImage, uint64(10000+w*150+i))
					}
					if err != nil && !errors.Is(err, ErrClosed) {
						t.Error(err)
						return
					}
				}
			}(w)
		}
		if err := s.EnsureMinimum(EntityImage, 100000); err != nil {
			t.Fatal(err)
		}
		if id, err := s.NextID(EntityImage); id <= 100000 || err != nil {
			t.Fatalf("floor: %d, %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if _, err := s.NextIDs(EntityImage, 8); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if err := s.EnsureMinimum(EntityImage, 200000); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	})
}
