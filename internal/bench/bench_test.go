package bench

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestDatasetsAreDeterministic(t *testing.T) {
	a, b := BitmapIDs(7, 1000, Sparse), BitmapIDs(7, 1000, Sparse)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("BitmapIDs differs at %d: %d vs %d", i, a[i], b[i])
		}
	}
	sparseA, sparseB := SparseIDs(5000, 1<<40, 9), SparseIDs(5000, 1<<40, 9)
	for i := range sparseA {
		if sparseA[i] != sparseB[i] {
			t.Fatalf("SparseIDs differs at %d", i)
		}
	}
	hashesA, hashesB := Hash32s(3, 64), Hash32s(3, 64)
	if hashesA[10] != hashesB[10] {
		t.Fatal("Hash32s differs")
	}
	if got := Timestamps(5, 1000, TimeClustered); len(got) != 1000 {
		t.Fatalf("timestamps length = %d", len(got))
	}
}

func TestDatasetsRespectIDZero(t *testing.T) {
	for _, ids := range [][]uint64{DenseIDs(100), SparseIDs(100, 1<<30, 1), BitmapIDs(1, 100, Medium)} {
		for _, id := range ids {
			if id == 0 {
				t.Fatal("generated reserved ImageID zero")
			}
		}
	}
}

func TestDensityCardinality(t *testing.T) {
	for _, d := range Densities() {
		ids := BitmapIDs(11, 1000, d)
		if len(ids) != 1000 {
			t.Fatalf("%s: cardinality = %d, want 1000", d, len(ids))
		}
	}
}

func TestHammingDistanceExact(t *testing.T) {
	rng := NewRNG(1)
	base := uint64(0xDEADBEEFCAFEF00D)
	for _, distance := range []int{0, 1, 8, 32, 64} {
		got := Hamming64(rng, base, distance)
		if want := popcount64(base ^ got); want != distance {
			t.Fatalf("Hamming64(%d) distance = %d", distance, want)
		}
	}
	pdq := [4]uint64{1, 2, 3, 4}
	for _, distance := range []int{0, 1, 33, 128, 256} {
		got := HammingPDQ(rng, pdq, distance)
		total := 0
		for w := range got {
			total += popcount64(got[w] ^ pdq[w])
		}
		if total != distance {
			t.Fatalf("HammingPDQ(%d) distance = %d", distance, total)
		}
	}
}

func popcount64(v uint64) int {
	n := 0
	for v != 0 {
		v &= v - 1
		n++
	}
	return n
}

func TestWorkloadBounds(t *testing.T) {
	for _, kind := range QueryKinds() {
		work := Workload(3, kind, 100, 1000)
		if len(work) != 1000 {
			t.Fatalf("%s length = %d", kind, len(work))
		}
		for _, idx := range work {
			if idx < 0 || idx >= 100 {
				t.Fatalf("%s index out of range: %d", kind, idx)
			}
		}
	}
	hot := HotSetWorkload(4, 1000, 500, 0.2, 0.8, nil)
	if len(hot) != 500 {
		t.Fatalf("hot workload length = %d", len(hot))
	}
}

func TestPlanCounts(t *testing.T) {
	plan := Plan(6, 1000, 20, 5)
	writes, deletes, reads := 0, 0, 0
	for _, op := range plan {
		switch op {
		case Write:
			writes++
		case Delete:
			deletes++
		default:
			reads++
		}
	}
	if writes != 200 || deletes != 50 || reads != 750 {
		t.Fatalf("plan = %dW %dD %dR", writes, deletes, reads)
	}
	if got := MixLabel(5, 5); got != "90R-5W-5D" {
		t.Fatalf("MixLabel = %q", got)
	}
}

func TestRecorderPercentiles(t *testing.T) {
	rec := NewRecorder(100)
	for i := 1; i <= 100; i++ {
		rec.Record(time.Duration(i) * time.Millisecond)
	}
	if got := rec.Percentile(0.50); got != 50*time.Millisecond {
		t.Fatalf("p50 = %v", got)
	}
	if got := rec.Percentile(0.99); got != 99*time.Millisecond {
		t.Fatalf("p99 = %v", got)
	}
	if got := rec.Max(); got != 100*time.Millisecond {
		t.Fatalf("max = %v", got)
	}
	empty := NewRecorder(0)
	if got := empty.Percentile(0.5); got != 0 {
		t.Fatalf("empty p50 = %v", got)
	}
}

func TestClosedLoopRunsEveryOp(t *testing.T) {
	rec := NewRecorder(64)
	var runs atomic.Int64
	done := ClosedLoop(64, 4, rec, func() { runs.Add(1) })
	if done != 64 || runs.Load() != 64 || rec.Count() != 64 {
		t.Fatalf("done=%d runs=%d recorded=%d", done, runs.Load(), rec.Count())
	}
}

func TestOpenLoopIssuesAtRate(t *testing.T) {
	rec := NewRecorder(64)
	var runs atomic.Int64
	issued := OpenLoop(20*time.Millisecond, 5000, 4, rec, func() { runs.Add(1) })
	if issued == 0 || runs.Load() != int64(issued) || rec.Count() != issued {
		t.Fatalf("issued=%d runs=%d recorded=%d", issued, runs.Load(), rec.Count())
	}
	if rec.Max() <= 0 {
		t.Fatal("latency not recorded")
	}
}

func TestSizesGating(t *testing.T) {
	if os.Getenv(largeEnv) != "" {
		t.Setenv(largeEnv, "")
	}
	sizes := Sizes(t)
	for _, n := range sizes {
		if n > 1_000_000 {
			t.Fatalf("large tier leaked into default sizes: %d", n)
		}
	}
	if len(sizes) == 0 {
		t.Fatal("no sizes")
	}
}
