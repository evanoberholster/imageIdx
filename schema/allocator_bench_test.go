package schema

import (
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
)

func benchOptions(backend Backend, namespaces int, block uint64) Options {
	d := NewDefinitions()
	for i := 0; i < namespaces; i++ {
		_ = d.Register(NamespaceDef{Namespace: Namespace(i + 1), Name: fmt.Sprintf("ns-%d", i+1), Kind: KindKV, Entity: EntityImage})
	}
	return Options{Backend: backend, Definitions: d, Entities: []EntityConfig{{Entity: EntityImage, ReserveSize: block}}}
}

func backendName(b Backend) string {
	if b == BackendMmap {
		return "mmap"
	}
	return "readwrite"
}

func BenchmarkNextID(b *testing.B) {
	for _, backend := range []Backend{BackendReadWrite, BackendMmap} {
		b.Run(backendName(backend), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "schema.dat"), benchOptions(backend, 1, 1<<32))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { b.StopTimer(); _ = s.Close() }()
			if _, err := s.NextID(1); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.NextID(1); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkNextIDs(b *testing.B) {
	for _, backend := range []Backend{BackendReadWrite, BackendMmap} {
		for _, count := range []uint64{1, 8, 64, 1024} {
			b.Run(fmt.Sprintf("%s/count=%d", backendName(backend), count), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "schema.dat"), benchOptions(backend, 1, math.MaxUint64))
				if err != nil {
					b.Fatal(err)
				}
				defer func() { b.StopTimer(); _ = s.Close() }()
				if _, err := s.NextIDs(1, 1); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.NextIDs(1, count); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkNextIDParallel(b *testing.B) {
	for _, backend := range []Backend{BackendReadWrite, BackendMmap} {
		for _, workers := range []int{1, 8, 32} {
			b.Run(fmt.Sprintf("%s/goroutines=%d", backendName(backend), workers), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "schema.dat"), benchOptions(backend, 1, 1<<32))
				if err != nil {
					b.Fatal(err)
				}
				defer func() { b.StopTimer(); _ = s.Close() }()
				if _, err := s.NextID(1); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				var wg sync.WaitGroup
				for w := 0; w < workers; w++ {
					wg.Add(1)
					go func(w int) {
						defer wg.Done()
						for i := w; i < b.N; i += workers {
							if _, err := s.NextID(1); err != nil {
								b.Error(err)
								return
							}
						}
					}(w)
				}
				wg.Wait()
			})
		}
	}
}

func BenchmarkNextIDsParallel(b *testing.B) {
	for _, backend := range []Backend{BackendReadWrite, BackendMmap} {
		b.Run(backendName(backend), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "schema.dat"), benchOptions(backend, 1, 1<<32))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { b.StopTimer(); _ = s.Close() }()
			if _, err := s.NextID(1); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := s.NextIDs(1, 8); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

func BenchmarkNamespaceCount(b *testing.B) {
	for _, n := range []int{1, 8, 16, 64, 256} {
		b.Run(fmt.Sprintf("namespaces=%d", n), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "schema.dat"), benchOptions(BackendReadWrite, n, 1<<32))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { b.StopTimer(); _ = s.Close() }()
			if _, ok := s.Lookup(Namespace(n)); !ok {
				b.Fatal("namespace missing")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := s.Lookup(Namespace(n)); !ok {
					b.Fatal("namespace missing")
				}
			}
		})
	}
}

func BenchmarkReservation(b *testing.B) {
	for _, backend := range []Backend{BackendReadWrite, BackendMmap} {
		for _, size := range []uint64{64, 256, 1024, 4096, 16384} {
			b.Run(fmt.Sprintf("%s/block=%d", backendName(backend), size), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "schema.dat"), benchOptions(backend, 1, size))
				if err != nil {
					b.Fatal(err)
				}
				defer func() { b.StopTimer(); _ = s.Close() }()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := s.EnsureMinimum(1, uint64(i+1)*size); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkOpenAndCleanClose(b *testing.B) {
	for _, backend := range []Backend{BackendReadWrite, BackendMmap} {
		b.Run(backendName(backend), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "schema.dat")
			opts := benchOptions(backend, 8, 1024)
			s, err := Open(path, opts)
			if err != nil {
				b.Fatal(err)
			}
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s, err := Open(path, opts)
				if err != nil {
					b.Fatal(err)
				}
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
