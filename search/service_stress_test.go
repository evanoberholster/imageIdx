package search

import (
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanoberholster/imageIdx/bitmapidx"

	"github.com/evanoberholster/imageIdx/timeidx"
)

const (
	stressReaders    = 100
	stressWriters    = 10
	stressReaderIter = 200
	// Small, frequent batches: measured 30x10 beats 10x30 here (102k vs
	// 47k q/s). Fuller batches hold the bitmap/geo writer locks longer
	// and publish larger deltas per round, which readers pay for on the
	// delta paths and at publication boundaries.
	stressWriteRound = 30
	stressBatchSize  = 10
)

type stressFixture struct {
	service *Service
	bitmap  *bitmapidx.Index
	times   *timeidx.Index
	geo     *bitmapidx.FieldIndex
	cell    bitmapidx.S2Key
	quantum uint64
}

func buildStressFixture(tb testing.TB) *stressFixture {
	tb.Helper()
	quantum := uint64(1) << timeidx.TimestampShift
	const initial = 6000

	bitmapIDs := make([]uint64, 0, initial/2)
	records := make([]timeidx.Entry, 0, initial)
	geoEntries := make([]bitmapidx.GeoEntry, 0, initial)
	for id := uint64(1); id <= initial; id++ {
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		records = append(records, timeidx.Entry{Timestamp: id * quantum, ImageID: ImageID(id)})
		geoEntries = append(geoEntries, bitmapidx.GeoEntry{ImageID: id, Latitude: 37, Longitude: -122})
	}

	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key:     bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1},
		IndexID: 1, IDs: bitmapIDs,
	}, bitmapidx.Config{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = bitmap.Close() })

	timeIndex, err := timeidx.New(timeidx.Spec{IndexID: 2, Entries: records}, timeidx.Config{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = timeIndex.Close() })

	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(3), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = geo.Close() })

	cell, err := bitmapidx.KeyFromLatLng(37, -122)
	if err != nil {
		tb.Fatal(err)
	}
	geoClause, err := WithinCells(geo, bitmapidx.DecodeS2Key(cell))
	if err != nil {
		tb.Fatal(err)
	}
	plan, err := Compile(
		AllBitmaps(bitmap),
		During(timeIndex, quantum, 20000*quantum),
		geoClause,
	)
	if err != nil {
		tb.Fatal(err)
	}
	return &stressFixture{
		service: NewService(plan, 8192, 32),
		bitmap:  bitmap, times: timeIndex, geo: geo,
		cell: cell, quantum: quantum,
	}
}

func checkSortedUnique(ids []ImageID) bool {
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			return false
		}
	}
	return true
}

// loadResult aggregates one concurrent run.
type loadResult struct {
	queries int64
	latency []int64 // nanoseconds, unsorted
	wall    time.Duration
}

// runLoad fires readers query agents and writers mutation agents at one
// shared Service. Every response must be sorted, unique, and non-empty;
// the first violation aborts via err.
func runLoad(fixture *stressFixture, readers, writers, iters, rounds, batchSize int) (loadResult, error) {
	service := fixture.service
	var totalQueries atomic.Int64
	latencies := make([][]int64, readers)
	errs := make(chan error, readers+writers)
	start := make(chan struct{})
	var readersDone, writersDone sync.WaitGroup

	for r := 0; r < readers; r++ {
		readersDone.Add(1)
		go func(r int) {
			defer readersDone.Done()
			<-start
			dst := make([]ImageID, 0, 8192)
			timings := make([]int64, 0, iters)
			for i := 0; i < iters; i++ {
				begin := time.Now()
				out := service.Query(dst[:0])
				timings = append(timings, int64(time.Since(begin)))
				dst = out[:0]
				if len(out) == 0 || !checkSortedUnique(out) {
					errs <- fmt.Errorf("reader %d iter %d: %d ids, sorted=%v", r, i, len(out), checkSortedUnique(out))
					return
				}
				totalQueries.Add(1)
			}
			latencies[r] = timings
		}(r)
	}

	for w := 0; w < writers; w++ {
		writersDone.Add(1)
		go func(w int) {
			defer writersDone.Done()
			<-start
			base := uint64(1_000_000 + w*100_000)
			for round := 0; round < rounds; round++ {
				ids := make([]uint64, 0, batchSize)
				for k := 0; k < batchSize; k++ {
					ids = append(ids, base+uint64(round*batchSize+k))
				}
				if err := fixture.bitmap.Apply(bitmapidx.Mutation{Add: ids}); err != nil {
					errs <- err
					return
				}
				batch := service.AcquireBatch()
				for k, id := range ids {
					if !batch.Append(timeidx.Insert(ImageID(id), fixture.quantum*uint64(7000+round*batchSize+k))) {
						service.ReleaseBatch(batch)
						errs <- fmt.Errorf("writer %d: batch full", w)
						return
					}
				}
				if err := fixture.times.Apply(batch); err != nil {
					service.ReleaseBatch(batch)
					errs <- err
					return
				}
				service.ReleaseBatch(batch)
				moves := make([]bitmapidx.Move, 0, len(ids))
				for _, id := range ids {
					moves = append(moves, bitmapidx.Move{ImageID: id, To: int64(fixture.cell)})
				}
				if err := fixture.geo.Move(moves...); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}

	wallStart := time.Now()
	close(start)
	writersDone.Wait()
	readersDone.Wait()
	wall := time.Since(wallStart)

	close(errs)
	for err := range errs {
		return loadResult{}, err
	}
	var all []int64
	for _, timings := range latencies {
		all = append(all, timings...)
	}
	return loadResult{queries: totalQueries.Load(), latency: all, wall: wall}, nil
}

func logLoadResult(tb testing.TB, name string, result loadResult, writers int) {
	tb.Helper()
	slices.Sort(result.latency)
	percentile := func(p float64) time.Duration {
		if len(result.latency) == 0 {
			return 0
		}
		return time.Duration(result.latency[int(p*float64(len(result.latency)-1))])
	}
	tb.Logf("[%s] queries=%d writers=%d wall=%v throughput=%.0f q/s",
		name, result.queries, writers, result.wall, float64(result.queries)/result.wall.Seconds())
	tb.Logf("[%s] latency p50=%v p99=%v max=%v",
		name, percentile(0.50), percentile(0.99), time.Duration(result.latency[len(result.latency)-1]))
}

// TestServiceConcurrentReadersWriters runs 100 query agents against one
// shared Service while 10 writer agents mutate all three indexes.
func TestServiceConcurrentReadersWriters(t *testing.T) {
	fixture := buildStressFixture(t)
	// Warm the workspace pool before the thundering herd so first queries
	// reuse scratch instead of growing it.
	fixture.service.Prewarm(stressReaders)

	result, err := runLoad(fixture, stressReaders, stressWriters, stressReaderIter, stressWriteRound, stressBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	logLoadResult(t, "100r/10w", result, stressWriters)
	t.Logf("final count=%d estimate=%d", fixture.service.Count(), fixture.service.Estimate())
}

// TestServiceReaderSweep varies reader pressure at fixed writer load to
// locate the oversubscription knee.
func TestServiceReaderSweep(t *testing.T) {
	for _, readers := range []int{12, 48} {
		fixture := buildStressFixture(t)
		fixture.service.Prewarm(readers)
		result, err := runLoad(fixture, readers, stressWriters, 100, stressWriteRound, stressBatchSize)
		if err != nil {
			t.Fatal(err)
		}
		logLoadResult(t, fmt.Sprintf("%dr/10w", readers), result, stressWriters)
	}
}

// TestServiceGOGCComparison reruns the load at GOGC=200 to isolate the
// allocator/GC contribution to tail latency.
func TestServiceGOGCComparison(t *testing.T) {
	fixture := buildStressFixture(t)
	fixture.service.Prewarm(stressReaders)
	base, err := runLoad(fixture, stressReaders, 0, 100, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	logLoadResult(t, "GOGC=100,no-writers", base, 0)

	previous := debug.SetGCPercent(200)
	hot, err := runLoad(fixture, stressReaders, 0, 100, 0, 0)
	debug.SetGCPercent(previous)
	if err != nil {
		t.Fatal(err)
	}
	logLoadResult(t, "GOGC=200,no-writers", hot, 0)
}

func BenchmarkServiceQuery(b *testing.B) {
	fixture := buildStressFixture(b)
	dst := make([]ImageID, 0, 8192)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dst = fixture.service.Query(dst[:0])
	}
}

func BenchmarkServiceQueryParallel(b *testing.B) {
	fixture := buildStressFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		dst := make([]ImageID, 0, 8192)
		for pb.Next() {
			dst = fixture.service.Query(dst[:0])
		}
	})
}

// BenchmarkServiceWorkspace isolates the pool: pooled workspaces against a
// freshly allocated Workspace per query.
func BenchmarkServiceWorkspace(b *testing.B) {
	fixture := buildStressFixture(b)
	dst := make([]ImageID, 0, 8192)
	b.Run("pooled", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			dst = fixture.service.Query(dst[:0])
		}
	})
	b.Run("fresh", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			dst = fixture.service.plan.Search(dst[:0], NewWorkspace(8192))
		}
	})
}

// BenchmarkServiceDstSizing isolates caller-buffer reuse: a pre-sized dst
// against a nil dst per query.
func BenchmarkServiceDstSizing(b *testing.B) {
	fixture := buildStressFixture(b)
	b.Run("sized", func(b *testing.B) {
		dst := make([]ImageID, 0, 8192)
		b.ReportAllocs()
		for range b.N {
			dst = fixture.service.Query(dst[:0])
		}
	})
	b.Run("nil", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = fixture.service.Query(nil)
		}
	})
}

// BenchmarkServiceStaleVsFreshPlan grows the indexes the way writers do,
// then compares the original (stale-estimate) plan against a recompiled one.
func BenchmarkServiceStaleVsFreshPlan(b *testing.B) {
	fixture := buildStressFixture(b)
	if _, err := runLoad(fixture, 0, stressWriters, 0, stressWriteRound, stressBatchSize); err != nil {
		b.Fatal(err)
	}
	geoClause, err := WithinCells(fixture.geo, bitmapidx.DecodeS2Key(fixture.cell))
	if err != nil {
		b.Fatal(err)
	}
	fresh, err := Compile(
		AllBitmaps(fixture.bitmap),
		During(fixture.times, fixture.quantum, 20000*fixture.quantum),
		geoClause,
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("stale stages=%+v", fixture.service.plan.Stages())
	b.Logf("fresh stages=%+v", fresh.Stages())
	for _, tc := range []struct {
		name string
		plan Plan
	}{
		{"stale", fixture.service.plan},
		{"fresh", fresh},
	} {
		b.Run(tc.name, func(b *testing.B) {
			workspace := NewWorkspace(8192)
			dst := make([]ImageID, 0, 8192)
			b.ReportAllocs()
			for range b.N {
				dst = tc.plan.Search(dst[:0], workspace)
			}
		})
	}
}
