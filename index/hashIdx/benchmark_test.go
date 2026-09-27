package hashidx

import (
	"fmt"
	"math/rand"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"

	"github.com/evanoberholster/imageIdx/roar"
)

// benchEntries derives deterministic entries so benchmarks are repeatable
// across machines and runs.
func benchEntries(count int) []Entry {
	entries := make([]Entry, count)
	state := uint64(0x123456789ABCDEF)
	next := func() uint64 {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}
	for k := range entries {
		entries[k] = Entry{
			ImageID: common.ImageID(k + 1),
			PHash64: next(),
			PDQ: PDQHash{
				next(), next(), next(), next(),
			},
		}
	}
	return entries
}

func buildBenchIndex(b *testing.B, count int, cfg Config) (*Index, []Entry) {
	b.Helper()
	entries := benchEntries(count)
	idx, err := newIndex(b.TempDir(), testPerceptualSpec(cfg), common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	if err := idx.Apply(entries); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })
	return idx, entries
}

func BenchmarkAppendSingle(b *testing.B) {
	idx, err := newIndex(b.TempDir(), testPerceptualSpec(Config{}), common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	entry := Entry{ImageID: 1, PDQ: pdqFrom(1)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entry.ImageID = common.ImageID(i + 1)
		entry.PHash64 = uint64(i)
		if err := idx.Add(entry); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "entries/sec")
}

func benchmarkApply(b *testing.B, batch int) {
	idx, err := newIndex(b.TempDir(), testPerceptualSpec(Config{}), common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	base := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries := benchEntries(batch)
		for k := range entries {
			entries[k].ImageID = common.ImageID(base + k + 1)
		}
		base += batch
		if err := idx.Apply(entries); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batch*b.N)/b.Elapsed().Seconds(), "entries/sec")
}

func BenchmarkApply100(b *testing.B)   { benchmarkApply(b, 100) }
func BenchmarkApply1000(b *testing.B)  { benchmarkApply(b, 1000) }
func BenchmarkApply10000(b *testing.B) { benchmarkApply(b, 10000) }

func BenchmarkSearchPHashRadiusFull(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, len(entries))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashRadius(dst[:0], query, 64, nil)
	}
	b.ReportMetric(float64(100_000*b.N)/b.Elapsed().Seconds(), "ordinals/sec")
}

func BenchmarkSearchPHashRadius0(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashRadius(dst[:0], query, 0, nil)
	}
}

func BenchmarkSearchPHashRadius2(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashRadius(dst[:0], query, 2, nil)
	}
}

func BenchmarkSearchPHashRadius5(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashRadius(dst[:0], query, 5, nil)
	}
}

func BenchmarkSearchPHashRadius10(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashRadius(dst[:0], query, 10, nil)
	}
}

func BenchmarkSearchPDQRadiusFull(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PDQ
	dst := make([]Result, 0, len(entries))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPDQRadius(dst[:0], query, 256, nil)
	}
	b.ReportMetric(float64(100_000*b.N)/b.Elapsed().Seconds(), "ordinals/sec")
}

func BenchmarkSearchPDQRadiusSmall(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PDQ
	dst := make([]Result, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPDQRadius(dst[:0], query, 32, nil)
	}
}

func BenchmarkSearchPDQRadiusBroad(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PDQ
	// Radius 128 admits roughly half of a random 256-bit population, so size
	// the destination for the full population to measure the scan, not growth.
	dst := make([]Result, 0, len(entries))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPDQRadius(dst[:0], query, 128, nil)
	}
}

func BenchmarkSearchPHashTop10(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, 10)
	workspace := &Workspace{}
	_ = idx.SearchPHashTopK(nil, query, 10, nil, workspace)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashTopK(dst[:0], query, 10, nil, workspace)
	}
}

func BenchmarkSearchPHashTop100(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PHash64
	dst := make([]Result, 0, 100)
	workspace := &Workspace{}
	_ = idx.SearchPHashTopK(nil, query, 100, nil, workspace)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPHashTopK(dst[:0], query, 100, nil, workspace)
	}
}

func BenchmarkSearchPDQTop10(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PDQ
	dst := make([]Result, 0, 10)
	workspace := &Workspace{}
	_ = idx.SearchPDQTopK(nil, query, 10, nil, workspace)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPDQTopK(dst[:0], query, 10, nil, workspace)
	}
}

func BenchmarkSearchPDQTop100(b *testing.B) {
	idx, entries := buildBenchIndex(b, 100_000, Config{})
	query := entries[0].PDQ
	dst := make([]Result, 0, 100)
	workspace := &Workspace{}
	_ = idx.SearchPDQTopK(nil, query, 100, nil, workspace)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.SearchPDQTopK(dst[:0], query, 100, nil, workspace)
	}
}

func BenchmarkCandidateDensity(b *testing.B) {
	adds := benchEntries(100_000)
	idx, err := newIndex(b.TempDir(), testPerceptualSpec(Config{}), common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if err := idx.Apply(adds); err != nil {
		b.Fatal(err)
	}
	query := adds[0].PDQ
	for _, percent := range []int{1, 10, 100} {
		limit := len(adds) * percent / 100
		ids := make([]uint64, limit)
		for k := range ids {
			ids[k] = uint64(k + 1)
		}
		candidates := roar.FromUnsorted(ids)
		var candidateSet CandidateSet = candidates
		dst := make([]Result, 0, 1024)
		b.Run(fmt.Sprintf("%d%%", percent), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = idx.SearchPDQRadius(dst[:0], query, 128, candidateSet)
			}
		})
	}
}

func BenchmarkRecordsPerPage(b *testing.B) {
	for _, recordsPerPage := range []uint32{4096, 8192, 16384, 65536} {
		b.Run(fmt.Sprintf("%d", recordsPerPage), func(b *testing.B) {
			idx, entries := buildBenchIndex(b, 100_000, Config{RecordsPerPage: recordsPerPage})
			rng := rand.New(rand.NewSource(1))
			dst := make([]Result, 0, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				entry := entries[rng.Intn(len(entries))]
				dst = idx.SearchPDQRadius(dst[:0], entry.PDQ, 64, nil)
			}
		})
	}
}

// BenchmarkCheckpoint measures the writer stall of a full rewrite plus
// manifest publish per 1,000-entry delta. Treat it as the write-stall
// budget probe: on reference hardware a delta checkpoint should stay
// well under an interactive maintenance window. Absolute ns/op moves
// with the machine; watch allocs/op (should stay flat) and the
// entries/sec trend across commits.
func BenchmarkCheckpoint(b *testing.B) {
	dir := b.TempDir()
	idx, err := newIndex(dir, testPerceptualSpec(Config{RecordsPerPage: 8192}), common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if err := idx.Apply(benchEntries(10_000)); err != nil {
		b.Fatal(err)
	}
	base := 10_000
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries := benchEntries(1_000)
		for k := range entries {
			entries[k].ImageID = common.ImageID(base + k + 1)
		}
		base += 1_000
		if err := idx.Apply(entries); err != nil {
			b.Fatal(err)
		}
		if err := idx.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(1_000*b.N)/b.Elapsed().Seconds(), "entries/sec")
}

// BenchmarkVacuum measures a full dense repack of 100k records. Same
// reading contract as BenchmarkCheckpoint: machine-dependent absolute
// time, but allocs/op and records/sec must not regress across commits.
func BenchmarkVacuum(b *testing.B) {
	dir := b.TempDir()
	cfg := Config{RecordsPerPage: 8192}
	idx, err := newIndex(dir, testPerceptualSpec(cfg), common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if err := idx.Apply(benchEntries(100_000)); err != nil {
		b.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Vacuum(nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(100_000*b.N)/b.Elapsed().Seconds(), "records/sec")
}

// benchHash derives a deterministic pseudorandom hash from n. It uses
// xorshift-style mixing so benchmark lookups scatter across blocks rather
// than scanning sequentially.
func benchHash(n uint64) Hash {
	x := n*0x9E3779B97F4A7C15 + 0xBF58476D1CE4E5B9
	x ^= x >> 29
	x *= 0x165667B19E3779F9
	x ^= x >> 32
	var h Hash
	for i := 0; i < 4; i++ {
		v := x + uint64(i)*0xD1B54A32D192ED03
		h[i*8+0] = byte(v >> 56)
		h[i*8+1] = byte(v >> 48)
		h[i*8+2] = byte(v >> 40)
		h[i*8+3] = byte(v >> 32)
		h[i*8+4] = byte(v >> 24)
		h[i*8+5] = byte(v >> 16)
		h[i*8+6] = byte(v >> 8)
		h[i*8+7] = byte(v)
	}
	return h
}

func buildExactBenchIndex(b *testing.B, count int, cfg ExactConfig, duplicates int) (*ExactIndex, []Hash) {
	b.Helper()
	entries := make([]ExactEntry, 0, count)
	hashes := make([]Hash, 0, count)
	id := common.ImageID(1)
	for i := 0; i < count; {
		h := benchHash(uint64(i))
		hashes = append(hashes, h)
		for d := 0; d < duplicates && i < count; d++ {
			entries = append(entries, ExactEntry{Hash: h, ImageID: id})
			id++
			i++
		}
		if duplicates == 0 {
			i++
		}
	}
	// Top up when duplicates grouping undershoots.
	for len(entries) < count {
		h := benchHash(uint64(len(entries) * 7919))
		hashes = append(hashes, h)
		entries = append(entries, ExactEntry{Hash: h, ImageID: id})
		id++
	}
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, cfg, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })
	return idx, hashes
}

// BenchmarkLookupHit1 pins the hot point-lookup cost: 0 allocs, and time
// must scale with the matching run length (see Hit4/Hit16/Spanning), not
// with index size.
func BenchmarkLookupHit1(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 100_000, ExactConfig{}, 1)
	h := hashes[len(hashes)/2]
	dst := make([]common.ImageID, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.Lookup(dst[:0], h)
	}
}

func BenchmarkLookupHit4(b *testing.B) {
	idx, _ := buildExactBenchIndex(b, 0, ExactConfig{}, 0)
	_ = idx.Close()
	h := benchHash(12345)
	var entries []ExactEntry
	for id := common.ImageID(1); id <= 4; id++ {
		entries = append(entries, ExactEntry{Hash: h, ImageID: id})
	}
	for i := 0; i < 20_000; i++ {
		entries = append(entries, ExactEntry{Hash: benchHash(uint64(1_000_000 + i)), ImageID: common.ImageID(100 + i)})
	}
	idx2, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx2.Close() })
	dst := make([]common.ImageID, 0, 8)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx2.Lookup(dst[:0], h)
	}
}

func BenchmarkLookupHit16(b *testing.B) {
	h := benchHash(999)
	var entries []ExactEntry
	for id := common.ImageID(1); id <= 16; id++ {
		entries = append(entries, ExactEntry{Hash: h, ImageID: id})
	}
	for i := 0; i < 20_000; i++ {
		entries = append(entries, ExactEntry{Hash: benchHash(uint64(2_000_000 + i)), ImageID: common.ImageID(1000 + i)})
	}
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })
	dst := make([]common.ImageID, 0, 16)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.Lookup(dst[:0], h)
	}
}

func BenchmarkLookupDuplicateSpanning(b *testing.B) {
	h := benchHash(7)
	var entries []ExactEntry
	for id := common.ImageID(1); id <= 40; id++ {
		entries = append(entries, ExactEntry{Hash: h, ImageID: id})
	}
	for i := 0; i < 5_000; i++ {
		entries = append(entries, ExactEntry{Hash: benchHash(uint64(3_000_000 + i)), ImageID: common.ImageID(10_000 + i)})
	}
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{BlockSize: 512}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })
	dst := make([]common.ImageID, 0, 40)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.Lookup(dst[:0], h)
	}
}

func BenchmarkLookupMiss(b *testing.B) {
	idx, _ := buildExactBenchIndex(b, 100_000, ExactConfig{}, 1)
	miss := benchHash(1<<62 + 1)
	dst := make([]common.ImageID, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.Lookup(dst[:0], miss)
	}
}

func BenchmarkLookupHotHit(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 10_000, ExactConfig{}, 1)
	h := hashes[0]
	// Warm the mapping/page cache.
	_ = idx.Lookup(nil, h)
	dst := make([]common.ImageID, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.Lookup(dst[:0], h)
	}
}

func BenchmarkLookupRandomHits(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 100_000, ExactConfig{}, 1)
	rng := rand.New(rand.NewSource(1))
	dst := make([]common.ImageID, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := hashes[rng.Intn(len(hashes))]
		dst = idx.Lookup(dst[:0], h)
	}
}

// BenchmarkContainsHot pins the hot membership cost alongside
// BenchmarkCompiledContains: one-shot stays 0-alloc, and the compiled
// form must hold its ~7x lead across commits.
func BenchmarkContainsHot(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 100_000, ExactConfig{}, 1)
	h := hashes[7]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Contains(h)
	}
}

func BenchmarkCountHot(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 100_000, ExactConfig{}, 1)
	h := hashes[7]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Count(h)
	}
}

func benchmarkBlockSize(b *testing.B, blockSize uint32) {
	cfg := ExactConfig{BlockSize: blockSize}
	idx, hashes := buildExactBenchIndex(b, 50_000, cfg, 1)
	rng := rand.New(rand.NewSource(42))
	dst := make([]common.ImageID, 0, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = idx.Lookup(dst[:0], hashes[rng.Intn(len(hashes))])
	}
}

func BenchmarkBlockSize16KiB(b *testing.B) { benchmarkBlockSize(b, 16*1024) }
func BenchmarkBlockSize32KiB(b *testing.B) { benchmarkBlockSize(b, 32*1024) }
func BenchmarkBlockSize64KiB(b *testing.B) { benchmarkBlockSize(b, 64*1024) }

func BenchmarkWriteSingle(b *testing.B) {
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Add(benchHash(uint64(i)), common.ImageID(i+1))
	}
}

func benchmarkWriteBatch(b *testing.B, batch int) {
	cfg := ExactConfig{MaxBatchEntries: uint32(batch)}
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256}, cfg, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	base := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := Mutation{}
		for k := 0; k < batch; k++ {
			n := base + k
			m.Add = append(m.Add, ExactEntry{Hash: benchHash(uint64(n)), ImageID: common.ImageID(n + 1)})
		}
		base += batch
		if err := idx.Apply(m); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(batch*b.N)/b.Elapsed().Seconds(), "entries/sec")
}

func BenchmarkWriteBatch10(b *testing.B)    { benchmarkWriteBatch(b, 10) }
func BenchmarkWriteBatch100(b *testing.B)   { benchmarkWriteBatch(b, 100) }
func BenchmarkWriteBatch1000(b *testing.B)  { benchmarkWriteBatch(b, 1000) }
func BenchmarkWriteBatch10000(b *testing.B) { benchmarkWriteBatch(b, 10000) }

func BenchmarkWriteOneBlock(b *testing.B) {
	// All entries share neighboring hashes so every batch hits one block.
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entriesForRange(0, 100, 1)}, ExactConfig{BlockSize: 4096}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	base := uint64(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := Mutation{}
		for k := 0; k < 100; k++ {
			n := base + uint64(k)
			// High hashes land in the last block.
			m.Add = append(m.Add, ExactEntry{Hash: benchHash(1<<60 + n), ImageID: common.ImageID(n + 1)})
		}
		base += 100
		if err := idx.Apply(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteSpread(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 20_000, ExactConfig{BlockSize: 1024}, 1)
	_ = hashes
	base := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := Mutation{}
		for k := 0; k < 100; k++ {
			n := base*100 + k
			m.Add = append(m.Add, ExactEntry{Hash: benchHash(uint64(9_000_000 + n)), ImageID: common.ImageID(50_000 + n)})
		}
		base++
		if err := idx.Apply(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteSplitHeavy(b *testing.B) {
	idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{BlockSize: 512, TargetFillPct: 80}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	base := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := Mutation{}
		for k := 0; k < 200; k++ {
			n := base*200 + k
			m.Add = append(m.Add, ExactEntry{Hash: benchHash(uint64(n)), ImageID: common.ImageID(n + 1)})
		}
		base++
		if err := idx.Apply(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteDeleteHeavy(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 20_000, ExactConfig{}, 1)
	_ = hashes
	rng := rand.New(rand.NewSource(7))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := benchHash(uint64(rng.Intn(20_000)))
		// Delete-then-add churn on nearby hashes.
		_ = idx.Remove(h, common.ImageID(rng.Intn(20_000)+1))
		_ = idx.Add(benchHash(uint64(50_000_000+i)), common.ImageID(100_000+i))
	}
}

func BenchmarkStorageAmplification(b *testing.B) {
	dir := b.TempDir()
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{BlockSize: 1024}, common.DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	for round := 0; round < 20; round++ {
		m := Mutation{}
		for k := 0; k < 500; k++ {
			n := round*500 + k
			m.Add = append(m.Add, ExactEntry{Hash: benchHash(uint64(n)), ImageID: common.ImageID(n + 1)})
		}
		if err := idx.Apply(m); err != nil {
			b.Fatal(err)
		}
		if err := idx.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
	// Fragment with deletes.
	m := Mutation{}
	for k := 0; k < 5_000; k++ {
		m.Remove = append(m.Remove, ExactEntry{Hash: benchHash(uint64(k * 2)), ImageID: common.ImageID(k*2 + 1)})
	}
	_ = idx.Apply(m)
	before := idx.StorageAmplification()
	beforeBytes := idx.OnDiskBytes()
	if err := idx.Vacuum(); err != nil {
		b.Fatal(err)
	}
	after := idx.StorageAmplification()
	afterBytes := idx.OnDiskBytes()
	b.ReportMetric(before, "ampl-before")
	b.ReportMetric(after, "ampl-after")
	b.ReportMetric(float64(beforeBytes), "bytes-before")
	b.ReportMetric(float64(afterBytes), "bytes-after")
	b.Logf("live=%d before=%d (%.2fx) after=%d (%.2fx)", idx.LiveBytes(), beforeBytes, before, afterBytes, after)
}

func BenchmarkHashSHA256(b *testing.B) {
	sizes := []int{64 << 10, 256 << 10, 1 << 20, 4 << 20}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			buf := make([]byte, size)
			rng := rand.New(rand.NewSource(99))
			_, _ = rng.Read(buf)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = SumSHA256(buf)
			}
		})
	}
}

func BenchmarkHashBLAKE3(b *testing.B) {
	sizes := []int{64 << 10, 256 << 10, 1 << 20, 4 << 20}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			buf := make([]byte, size)
			rng := rand.New(rand.NewSource(99))
			_, _ = rng.Read(buf)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = SumBLAKE3(buf)
			}
		})
	}
}

// BenchmarkPublishScalingVsBlocks times one add plus one remove of a
// fresh pair (net-zero growth, so the directory size holds steady) at
// several index sizes. Staged publishes share the block slice, so cost
// must stay flat as the directory grows; only threshold folds scale with
// block count.
func BenchmarkPublishScalingVsBlocks(b *testing.B) {
	for _, total := range []int{2_000, 20_000} {
		b.Run(fmt.Sprintf("entries=%d", total), func(b *testing.B) {
			entries := make([]ExactEntry, 0, total)
			for i := 0; i < total; i++ {
				entries = append(entries, ExactEntry{Hash: benchHash(uint64(i)), ImageID: common.ImageID(i + 1)})
			}
			idx, err := newExact(b.TempDir(), ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries},
				ExactConfig{BlockSize: 512}, common.DurabilityOS)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = idx.Close() }()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n := uint64(total + i)
				e := ExactEntry{Hash: benchHash(n), ImageID: common.ImageID(n + 1)}
				if err := idx.Add(e.Hash, e.ImageID); err != nil {
					b.Fatal(err)
				}
				if err := idx.Remove(e.Hash, e.ImageID); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCompiledContains measures a generation-gated compiled lookup
// against the one-shot path. The clean sub-bench replays a cached run;
// the staged sub-bench holds ~400 overlay entries to price the probe.
func BenchmarkCompiledContains(b *testing.B) {
	idx, hashes := buildExactBenchIndex(b, 100_000, ExactConfig{}, 1)
	h := hashes[7]
	b.Run("clean", func(b *testing.B) {
		c := idx.CompileLookup(h)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if !c.Contains() {
				b.Fatal("miss")
			}
		}
	})
	b.Run("staged", func(b *testing.B) {
		for i := 0; i < 400; i++ {
			if err := idx.Add(benchHash(uint64(50_000_000+i)), common.ImageID(200_000+i)); err != nil {
				b.Fatal(err)
			}
		}
		c := idx.CompileLookup(h)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if !c.Contains() {
				b.Fatal("miss")
			}
		}
	})
}
