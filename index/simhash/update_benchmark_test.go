package simhash

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
)

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
			dst := make([]PDQMatch, 0, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQ(dst[:0], queries[i%len(queries)], 16)
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
