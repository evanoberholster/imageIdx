package simhash

import (
	"fmt"
	common "github.com/evanoberholster/imageIdx/common"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkSeqRetry compares reader retry policies under a writer that holds the
// sequence odd in short bursts. The loops are inlined so the result measures the
// policy, not function-call overhead.
func BenchmarkSeqRetry(b *testing.B) {
	var seq uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			atomic.StoreUint64(&seq, 1)
			for i := 0; i < 32; i++ {
				runtime.Gosched()
			}
			atomic.StoreUint64(&seq, 0)
			for i := 0; i < 32; i++ {
				runtime.Gosched()
			}
		}
	}()
	b.Run("immediateSpin", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for atomic.LoadUint64(&seq)&1 != 0 {
			}
		}
	})
	b.Run("boundedSpin", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			spin := 0
			for atomic.LoadUint64(&seq)&1 != 0 {
				spin++
				if spin >= seqSpinLimit {
					runtime.Gosched()
				}
			}
		}
	})
	b.Run("immediateGosched", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for atomic.LoadUint64(&seq)&1 != 0 {
				runtime.Gosched()
			}
		}
	})
	close(stop)
	wg.Wait()
}

func benchSetup(b *testing.B, n, seq int) (*Store, []Record) {
	b.Helper()
	records := randRecords(int64(n), n)
	st, err := Open(b.TempDir(), Config{
		MaxShardBytes:   shardBytes(256, seq),
		SeqBlockRecords: seq,
	})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(records); err != nil {
		b.Fatalf("AppendBatch: %v", err)
	}
	return st, records
}

func benchQueries(records []Record, count, distance int) []PDQHash {
	rng := rand.New(rand.NewSource(99))
	out := make([]PDQHash, count)
	for i := range out {
		out[i] = flipPDQ(rng, records[(i*7919)%len(records)].PDQ, distance)
	}
	return out
}

// searchBenchSizes returns the dataset sizes required for the search latency
// suite. The standard matrix is covered by the other benchmarks.
func searchBenchSizes(b *testing.B) []int {
	b.Helper()
	if testing.Short() {
		return []int{10_000, 64_000}
	}
	return []int{64_000, 1_000_000}
}

func BenchmarkSearchPDQ(b *testing.B) {
	for _, n := range searchBenchSizes(b) {
		st, records := benchSetup(b, n, defaultSeqBlockRecords)
		queries := benchQueries(records, 64, 12)
		for _, maxD := range []uint8{0, 8, 16, 31, 32, 40, 50, 64, 96, 128} {
			b.Run(fmt.Sprintf("n=%d/d=%d", n, maxD), func(b *testing.B) {
				dst := make([]Result, 0, n)
				b.ReportAllocs()
				b.ResetTimer()
				start := time.Now()
				for i := 0; i < b.N; i++ {
					dst = st.SearchPDQ(dst[:0], &queries[i%len(queries)], maxD)
				}
				elapsed := time.Since(start)
				b.StopTimer()
				b.ReportMetric(float64(elapsed.Nanoseconds())/float64(int64(b.N)*int64(n)), "ns/hash")
			})
		}
		st.Close()
	}
}

func BenchmarkSearchPHash(b *testing.B) {
	for _, n := range searchBenchSizes(b) {
		st, records := benchSetup(b, n, defaultSeqBlockRecords)
		rng := rand.New(rand.NewSource(7))
		queries := make([]uint64, 64)
		for i := range queries {
			queries[i] = flipPHash(rng, records[(i*7919)%n].PHash, 8)
		}
		for _, maxD := range []uint8{8, 16, 32} {
			b.Run(fmt.Sprintf("n=%d/d=%d", n, maxD), func(b *testing.B) {
				dst := make([]Result, 0, n)
				b.ReportAllocs()
				b.ResetTimer()
				start := time.Now()
				for i := 0; i < b.N; i++ {
					dst = st.SearchPHash(dst[:0], queries[i%len(queries)], maxD)
				}
				elapsed := time.Since(start)
				b.StopTimer()
				b.ReportMetric(float64(elapsed.Nanoseconds())/float64(int64(b.N)*int64(n)), "ns/hash")
			})
		}
		st.Close()
	}
}

func BenchmarkUpsert(b *testing.B) {
	st, records := benchSetup(b, 100_000, defaultSeqBlockRecords)
	defer st.Close()
	rec := records[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.Upsert(uint64(i%100_000+1), rec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendBatch64(b *testing.B) {
	batch := randRecords(41, 64)
	st, err := Open(b.TempDir(), Config{MaxShardBytes: shardBytes(256, defaultSeqBlockRecords)})
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.AppendBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSeqBlockRecords(b *testing.B) {
	const n = 100_000
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		st, records := benchSetup(b, n, seq)
		queries := benchQueries(records, 64, 12)
		b.Run(fmt.Sprintf("seq=%d", seq), func(b *testing.B) {
			dst := make([]Result, 0, 256)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQ(dst[:0], &queries[i%len(queries)], 16)
			}
		})
		st.Close()
	}
}

func BenchmarkOpen(b *testing.B) {
	dir := b.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(64, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := st.AppendBatch(randRecords(51, 100_000)); err != nil {
		b.Fatal(err)
	}
	if err := st.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	if err := st.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reopened, err := Open(dir, cfg)
		if err != nil {
			b.Fatal(err)
		}
		if err := reopened.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCheckpoint(b *testing.B) {
	st, _ := benchSetup(b, 100_000, defaultSeqBlockRecords)
	defer st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

// resetCommitted rewinds the append watermark so append benchmarks reuse the
// same slots without creating shards. It is a benchmark helper only.
func resetCommitted(st *Store) {
	st.mu.Lock()
	st.committed = 0
	st.committedAtomic.Store(0)
	for _, sh := range st.shards {
		sh.committed = 0
	}
	st.mu.Unlock()
}

func BenchmarkAppendSingle(b *testing.B) {
	cfg := Config{MaxShardBytes: shardBytes(8, defaultSeqBlockRecords)}
	st, err := Open(b.TempDir(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	rec := randRecords(900, 1)[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resetCommitted(st)
		b.StartTimer()
		if _, err := st.Append(rec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendBatch(b *testing.B) {
	cfg := Config{MaxShardBytes: shardBytes(8, defaultSeqBlockRecords)}
	st, err := Open(b.TempDir(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	records := randRecords(901, optimizedBatchRecords)
	for _, size := range []int{1, 4, 16, 32, 64, 128, 256} {
		batch := records[:size]
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetCommitted(st)
				b.StartTimer()
				if _, err := st.AppendBatch(batch); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(size), "updates/op")
		})
	}
}

// setCommittedForBench positions the append watermark and ensures its shards
// exist, so a boundary benchmark starts at a specific ImageID.
func setCommittedForBench(st *Store, id uint64) {
	st.mu.Lock()
	_ = st.ensureShardsLocked(hashPageOf(id) / st.pagesPerShard)
	st.committed = id
	st.committedAtomic.Store(id)
	st.mu.Unlock()
}

// BenchmarkAppendBoundary measures AppendBatch(64) at page, sequence-block, and
// shard boundaries versus within a page.
func BenchmarkAppendBoundary(b *testing.B) {
	cfg := Config{MaxShardBytes: shardBytes(8, defaultSeqBlockRecords)}
	st, err := Open(b.TempDir(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	recs := randRecords(950, optimizedBatchRecords)
	pps := st.PagesPerShard()
	cases := []struct {
		name  string
		start uint64
	}{
		{"withinPage", slotsPerHashPage / 2},
		{"pageBoundary", slotsPerHashPage - 32},
		{"seqBlockBoundary", defaultSeqBlockRecords - 32},
		{"shardBoundary", pps*slotsPerHashPage - optimizedBatchRecords},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				setCommittedForBench(st, c.start)
				b.StartTimer()
				if _, err := st.AppendBatch(recs); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(64, "updates/op")
		})
	}
}

func BenchmarkGet(b *testing.B) {
	st, _ := benchSetup(b, 100_000, defaultSeqBlockRecords)
	defer st.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := st.Get(common.ImageID(1 + i%100_000)); !ok {
			b.Fatal("missing")
		}
	}
}

func BenchmarkAddressing(b *testing.B) {
	var sink uint64
	for i := 0; i < b.N; i++ {
		id := uint64(i%1_000_000) + 1
		page := hashPageOf(id)
		slot := slotOf(id)
		sink = page + uint64(slot)
	}
	_ = sink
}

// upsertBatchFixedSorted is a benchmark-only alternative grouping that
// insertion-sorts up to optimizedBatchRecords indices in a fixed array by
// ImageID. It is compared against the shipped consecutive-run grouping.
func upsertBatchFixedSorted(st *Store, updates []Update) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(updates) == 0 {
		return nil
	}
	if len(updates) > optimizedBatchRecords {
		return ErrBatchTooLarge
	}
	if err := st.journal.appendBatch(st.generation, st.generation+1, updates, false); err != nil {
		return err
	}
	var order [optimizedBatchRecords]uint8
	for i := range updates {
		order[i] = uint8(i)
	}
	for a := 1; a < len(updates); a++ {
		v := order[a]
		bb := a - 1
		for bb >= 0 && updates[order[bb]].ImageID > updates[v].ImageID {
			order[bb+1] = order[bb]
			bb--
		}
		order[bb+1] = v
	}
	i := 0
	for i < len(updates) {
		page := hashPageOf(uint64(updates[order[i]].ImageID))
		block := st.geo.blockOf(slotOf(uint64(updates[order[i]].ImageID)))
		j := i + 1
		for j < len(updates) {
			id := uint64(updates[order[j]].ImageID)
			if hashPageOf(id) != page || st.geo.blockOf(slotOf(id)) != block {
				break
			}
			j++
		}
		sh := st.shards[page/st.pagesPerShard]
		pi := page - sh.firstPage
		seq := sh.seqPtr(pi, block)
		atomic.AddUint64(seq, 1)
		for k := i; k < j; k++ {
			idx := order[k]
			st.applyPointNoSeq(sh, pi, slotOf(uint64(updates[idx].ImageID)), updates[idx].Record)
		}
		atomic.AddUint64(seq, 1)
		i = j
	}
	st.generation++
	st.gen.Store(st.generation)
	return nil
}

func BenchmarkGroupingExperiment(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	rng := rand.New(rand.NewSource(31))
	local := make([]Update, 64)
	random := make([]Update, 64)
	for i := range local {
		local[i] = Update{ImageID: common.ImageID(50_000 + i), Record: records[i]}
		random[i] = Update{ImageID: common.ImageID(1 + rng.Intn(1_000_000)), Record: records[i]}
	}
	b.Run("consecutive/local", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := st.UpsertBatch(local); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("fixedSorted/local", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := upsertBatchFixedSorted(st, local); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("consecutive/random", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := st.UpsertBatch(random); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("fixedSorted/random", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := upsertBatchFixedSorted(st, random); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSearchPDQBulkVsScalar compares the SIMD block kernel (quiescent
// bulk scan) with the concurrent-safe scalar scan.
func BenchmarkSearchPDQBulkVsScalar(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	queries := benchQueries(records, 64, 12)
	for _, maxD := range []uint8{0, 8, 16, 31, 32, 40, 50, 64, 96, 128} {
		b.Run(fmt.Sprintf("scalar/d=%d", maxD), func(b *testing.B) {
			dst := make([]Result, 0, len(records))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQ(dst[:0], &queries[i%len(queries)], maxD)
			}
		})
		b.Run(fmt.Sprintf("bulk/d=%d", maxD), func(b *testing.B) {
			dst := make([]Result, 0, len(records))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQBulk(dst[:0], &queries[i%len(queries)], maxD)
			}
		})
	}
}

// applyPointLocked is the benchmark-only per-record update path used to compare
// per-record and grouped sequence transitions.
func (st *Store) applyPointLocked(id uint64, rec Record) {
	page := hashPageOf(id)
	sh := st.shards[page/st.pagesPerShard]
	pi := page - sh.firstPage
	seq := sh.seqPtr(pi, st.geo.blockOf(slotOf(id)))
	atomic.AddUint64(seq, 1)
	st.applyPointNoSeq(sh, pi, slotOf(id), rec)
	atomic.AddUint64(seq, 1)
}

func reportNsPerUpdate(b *testing.B, updates int, elapsed time.Duration) {
	b.Helper()
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(int64(b.N)*int64(updates)), "ns/update")
}

func makeUpdates(records []Record, ids []common.ImageID) []Update {
	updates := make([]Update, len(ids))
	for i := range ids {
		updates[i] = Update{ImageID: ids[i], Record: records[i%len(records)]}
	}
	return updates
}

// BenchmarkUpsertBatch measures batch latency and per-update cost.
func BenchmarkUpsertBatch(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	base := common.ImageID(200_000)
	for _, size := range []int{1, 4, 16, 32, 64, 128, 256} {
		ids := make([]common.ImageID, size)
		for i := range ids {
			ids[i] = base + common.ImageID(i*997)
		}
		updates := makeUpdates(records, ids)
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			start := time.Now()
			for i := 0; i < b.N; i++ {
				if err := st.UpsertBatch(updates); err != nil {
					b.Fatal(err)
				}
			}
			elapsed := time.Since(start)
			b.StopTimer()
			reportNsPerUpdate(b, size, elapsed)
		})
	}
}

// BenchmarkUpsert64VsBatch64 compares 64 single Upserts with one batch.
func BenchmarkUpsert64VsBatch64(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	ids := make([]common.ImageID, 64)
	for i := range ids {
		ids[i] = 200_000 + common.ImageID(i*997)
	}
	updates := makeUpdates(records, ids)
	b.Run("individual64", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		start := time.Now()
		for i := 0; i < b.N; i++ {
			for j := range updates {
				if err := st.Upsert(updates[j].ImageID, updates[j].Record); err != nil {
					b.Fatal(err)
				}
			}
		}
		elapsed := time.Since(start)
		b.StopTimer()
		reportNsPerUpdate(b, 64, elapsed)
	})
	b.Run("batch64", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		start := time.Now()
		for i := 0; i < b.N; i++ {
			if err := st.UpsertBatch(updates); err != nil {
				b.Fatal(err)
			}
		}
		elapsed := time.Since(start)
		b.StopTimer()
		reportNsPerUpdate(b, 64, elapsed)
	})
}

// BenchmarkUpsertBatchLocality exercises update locality patterns.
func BenchmarkUpsertBatchLocality(b *testing.B) {
	const n = 1_000_000
	st, records := benchSetup(b, n, defaultSeqBlockRecords)
	defer st.Close()
	rng := rand.New(rand.NewSource(7))
	base := uint64(200_000)
	patterns := []struct {
		name string
		pick func(i int) common.ImageID
	}{
		{"sequential", func(i int) common.ImageID { return common.ImageID(1 + (base+uint64(i))%n) }},
		{"sameBlock", func(i int) common.ImageID { return common.ImageID(1 + base + uint64(i%1000)) }},
		{"samePage", func(i int) common.ImageID { return common.ImageID(1 + (base/65536)*65536 + uint64((i*997)%65536)) }},
		{"spreadPages", func(i int) common.ImageID { return common.ImageID(1 + (base+uint64(i)*65536)%n) }},
		{"random", func(i int) common.ImageID { return common.ImageID(1 + rng.Intn(n)) }},
	}
	for _, p := range patterns {
		ids := make([]common.ImageID, 64)
		for i := range ids {
			ids[i] = p.pick(i)
		}
		updates := makeUpdates(records, ids)
		b.Run(p.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			start := time.Now()
			for i := 0; i < b.N; i++ {
				if err := st.UpsertBatch(updates); err != nil {
					b.Fatal(err)
				}
			}
			elapsed := time.Since(start)
			b.StopTimer()
			reportNsPerUpdate(b, 64, elapsed)
		})
	}
}

// upsertBatchUngrouped is a benchmark-only variant that toggles the seqlock once
// per record instead of once per sequence block.
func upsertBatchUngrouped(st *Store, updates []Update) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.journal.appendBatch(st.generation, st.generation+1, updates, false); err != nil {
		return err
	}
	for i := range updates {
		st.applyPointLocked(uint64(updates[i].ImageID), updates[i].Record)
	}
	st.generation++
	st.gen.Store(st.generation)
	return nil
}

// BenchmarkSeqGrouping compares grouped and per-record sequence transitions.
func BenchmarkSeqGrouping(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	rng := rand.New(rand.NewSource(11))
	for _, size := range []int{1, 4, 16, 64} {
		local := make([]Update, size)
		random := make([]Update, size)
		for i := range local {
			local[i] = Update{ImageID: common.ImageID(200_000 + i), Record: records[i]}
			random[i] = Update{ImageID: common.ImageID(1 + rng.Intn(1_000_000)), Record: records[i]}
		}
		b.Run(fmt.Sprintf("grouped/sameBlock/%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := st.UpsertBatch(local); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("ungrouped/sameBlock/%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := upsertBatchUngrouped(st, local); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("grouped/random/%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := st.UpsertBatch(random); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("ungrouped/random/%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := upsertBatchUngrouped(st, random); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWALBatch isolates WAL encode/append cost and bytes written.
func BenchmarkWALBatch(b *testing.B) {
	st, _ := benchSetup(b, 8, defaultSeqBlockRecords)
	defer st.Close()
	records := randRecords(200, 64)
	for _, size := range []int{1, 16, 32, 64, 128, 256} {
		updates := makeUpdates(records, make([]common.ImageID, size))
		for i := range updates {
			updates[i].ImageID = common.ImageID(i + 1)
		}
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			before, _ := st.journal.wal.Offset()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := st.journal.appendBatch(uint64(i), uint64(i)+1, updates, false); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			after, _ := st.journal.wal.Offset()
			b.ReportMetric(float64(after-before)/float64(b.N), "wal-B/op")
		})
	}
	// 64 individual frames versus one 64-update frame.
	single := makeUpdates(records[:1], []common.ImageID{1})
	b.Run("individual64", func(b *testing.B) {
		before, _ := st.journal.wal.Offset()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for j := 0; j < 64; j++ {
				if err := st.journal.appendBatch(uint64(j), uint64(j)+1, single, false); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.StopTimer()
		after, _ := st.journal.wal.Offset()
		b.ReportMetric(float64(after-before)/float64(b.N), "wal-B/64updates")
	})
}

// BenchmarkDurabilityModes separates relaxed and sync update batches.
func BenchmarkDurabilityModes(b *testing.B) {
	for _, mode := range []struct {
		name string
		dur  common.Durability
	}{{"relaxed", common.DurabilityOS}, {"sync", common.DurabilitySync}} {
		cfg := testConfig()
		cfg.Durability = mode.dur
		st, records := func() (*Store, []Record) {
			records := randRecords(300, 100_000)
			s, err := Open(b.TempDir(), cfg)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := s.AppendBatch(records); err != nil {
				b.Fatal(err)
			}
			return s, records
		}()
		for _, size := range []int{1, 16, 64} {
			ids := make([]common.ImageID, size)
			for i := range ids {
				ids[i] = 1000 + common.ImageID(i*997)
			}
			updates := makeUpdates(records, ids)
			b.Run(fmt.Sprintf("%s/batch=%d", mode.name, size), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				start := time.Now()
				for i := 0; i < b.N; i++ {
					if err := st.UpsertBatch(updates); err != nil {
						b.Fatal(err)
					}
				}
				elapsed := time.Since(start)
				b.StopTimer()
				reportNsPerUpdate(b, size, elapsed)
			})
		}
		st.Close()
	}
}

// BenchmarkCheckpointAfterUpdates measures checkpoint cost and WAL size.
func BenchmarkCheckpointAfterUpdates(b *testing.B) {
	for _, updates := range []int{0, 1, 64, 1024, 10_000} {
		b.Run(fmt.Sprintf("updates=%d", updates), func(b *testing.B) {
			dir := b.TempDir()
			cfg := testConfig()
			st, err := Open(dir, cfg)
			if err != nil {
				b.Fatal(err)
			}
			records := randRecords(400, 20_000)
			if _, err := st.AppendBatch(records); err != nil {
				b.Fatal(err)
			}
			if err := st.Checkpoint(); err != nil {
				b.Fatal(err)
			}
			for i := 0; i < updates; i++ {
				id := common.ImageID(1 + (i*7919)%20_000)
				if err := st.Upsert(id, records[i%len(records)]); err != nil {
					b.Fatal(err)
				}
			}
			if fi, err := journalSize(dir); err == nil {
				b.ReportMetric(float64(fi), "wal-B/before")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := st.Checkpoint(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			st.Close()
		})
	}
}

// BenchmarkRecovery measures Open replay cost for a WAL with N updates.
func BenchmarkRecovery(b *testing.B) {
	for _, updates := range []int{0, 1, 64, 1024, 10_000} {
		b.Run(fmt.Sprintf("updates=%d", updates), func(b *testing.B) {
			dir := b.TempDir()
			cfg := testConfig()
			st, err := Open(dir, cfg)
			if err != nil {
				b.Fatal(err)
			}
			records := randRecords(500, 20_000)
			if _, err := st.AppendBatch(records); err != nil {
				b.Fatal(err)
			}
			if err := st.Checkpoint(); err != nil {
				b.Fatal(err)
			}
			batch := make([]Update, 64)
			for i := 0; i < updates; i += 64 {
				for j := range batch {
					id := common.ImageID(1 + ((i+j)*7919)%20_000)
					batch[j] = Update{ImageID: id, Record: records[(i+j)%len(records)]}
				}
				if err := st.UpsertBatch(batch); err != nil {
					b.Fatal(err)
				}
			}
			if err := st.Close(); err != nil { // no checkpoint: WAL replays
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reopened, err := Open(dir, cfg)
				if err != nil {
					b.Fatal(err)
				}
				if err := reopened.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkConcurrentSearchUpdate measures search latency while updates run.
func BenchmarkConcurrentSearchUpdate(b *testing.B) {
	st, records := benchSetup(b, 200_000, defaultSeqBlockRecords)
	defer st.Close()
	queries := benchQueries(records, 8, 12)
	for _, size := range []int{1, 16, 64} {
		ids := make([]common.ImageID, size)
		for i := range ids {
			ids[i] = 1000 + common.ImageID(i*997)
		}
		updates := makeUpdates(records, ids)
		b.Run(fmt.Sprintf("batch=%d", size), func(b *testing.B) {
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := st.UpsertBatch(updates); err != nil {
						return
					}
				}
			}()
			dst := make([]Result, 0, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQ(dst[:0], &queries[i%len(queries)], 16)
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
		})
	}
}

// BenchmarkUpsertBatchDuplicates ensures duplicate ImageIDs do not take an
// unexpected slow path (no map is used; last-wins falls out of applying in
// order within a block group).
func BenchmarkUpsertBatchDuplicates(b *testing.B) {
	st, records := benchSetup(b, 1_000_000, defaultSeqBlockRecords)
	defer st.Close()
	updates := make([]Update, 64)
	for i := range updates {
		updates[i] = Update{ImageID: common.ImageID(1 + i%2), Record: records[i]}
	}
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if err := st.UpsertBatch(updates); err != nil {
			b.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	b.StopTimer()
	reportNsPerUpdate(b, 64, elapsed)
}

// BenchmarkSeqBlockSizeUpdates extends the block-size benchmark to updates.
func BenchmarkSeqBlockSizeUpdates(b *testing.B) {
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		st, records := benchSetup(b, 200_000, seq)
		rng := rand.New(rand.NewSource(21))
		local := make([]Update, 64)
		random := make([]Update, 64)
		for i := range local {
			local[i] = Update{ImageID: common.ImageID(50_000 + i), Record: records[i]}
			random[i] = Update{ImageID: common.ImageID(1 + rng.Intn(200_000)), Record: records[i]}
		}
		b.Run(fmt.Sprintf("local/seq=%d", seq), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := st.UpsertBatch(local); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("random/seq=%d", seq), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if err := st.UpsertBatch(random); err != nil {
					b.Fatal(err)
				}
			}
		})
		st.Close()
	}
}
