package simhash

import (
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

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
