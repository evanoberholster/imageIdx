package simhash

import (
	"fmt"
	"math/rand"
	"testing"

	bench "github.com/evanoberholster/imageIdx/internal/bench"
)

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

func BenchmarkSearchPDQ(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		st, records := benchSetup(b, n, defaultSeqBlockRecords)
		queries := benchQueries(records, 64, 12)
		for _, maxD := range []uint16{0, 4, 8, 16, 31, 32, 40, 50} {
			b.Run(fmt.Sprintf("n=%d/d=%d", n, maxD), func(b *testing.B) {
				dst := make([]PDQMatch, 0, 256)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					dst = st.SearchPDQ(dst[:0], queries[i%len(queries)], maxD)
				}
			})
		}
		st.Close()
	}
}

func BenchmarkSearchPHash(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		st, records := benchSetup(b, n, defaultSeqBlockRecords)
		rng := rand.New(rand.NewSource(7))
		queries := make([]uint64, 64)
		for i := range queries {
			queries[i] = flipPHash(rng, records[(i*7919)%n].PHash, 8)
		}
		for _, maxD := range []int{0, 4, 8, 16, 32, 64} {
			b.Run(fmt.Sprintf("n=%d/d=%d", n, maxD), func(b *testing.B) {
				dst := make([]PHashMatch, 0, 256)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					dst = st.SearchPHash(dst[:0], queries[i%len(queries)], maxD)
				}
			})
		}
		st.Close()
	}
}

func BenchmarkSearchCombined(b *testing.B) {
	for _, n := range bench.Sizes(b) {
		st, records := benchSetup(b, n, defaultSeqBlockRecords)
		rng := rand.New(rand.NewSource(17))
		phashQueries := make([]uint64, 64)
		pdqQueries := make([]PDQHash, 64)
		for i := range phashQueries {
			phashQueries[i] = flipPHash(rng, records[(i*7919)%n].PHash, 6)
			pdqQueries[i] = flipPDQ(rng, records[(i*7919)%n].PDQ, 12)
		}
		for _, order := range []Order{PHashFirst, PDQFirst} {
			b.Run(fmt.Sprintf("n=%d/order=%d", n, order), func(b *testing.B) {
				dst := make([]CombinedMatch, 0, 256)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					dst = st.SearchCombined(dst[:0], phashQueries[i%64], 8, pdqQueries[i%64], 16, order)
				}
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
			dst := make([]PDQMatch, 0, 256)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = st.SearchPDQ(dst[:0], queries[i%len(queries)], 16)
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
