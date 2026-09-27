package vectoridx

import (
	"math/rand"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// BenchmarkAddSyncParallel measures concurrent DurabilitySync inserts. The
// fsync runs after the writer releases writeMu, so it overlaps the next
// writer's HNSW insertion instead of serializing every insert behind it.
func BenchmarkAddSyncParallel(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.idx")
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		b.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(83))
	vec := randFloatVec(rng, Dim512)
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := idx.AddFloat32(NodeID(next.Add(1)+1_000_000), vec); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
