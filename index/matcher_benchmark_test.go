package index_test

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
)

type matcherBench struct {
	db       *index.Store
	m        *index.Matcher
	sha      []radixhash.Hash
	blake    []radixhash.Hash
	phash    []uint64
	pdq      []simhash.PDQHash
	shaIdx   *radixhash.Index
	blakeIdx *radixhash.Index
	sim      *simhash.Store
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

func benchHash(seed uint64, tag byte) radixhash.Hash {
	var h radixhash.Hash
	h[0] = tag
	for i := 1; i < 32; i++ {
		h[i] = byte(seed >> (8 * uint((i-1)%8)))
	}
	return h
}

func newMatcherBench(b *testing.B, n int) *matcherBench {
	b.Helper()
	db, err := index.Create(b.TempDir(), index.Spec{Schema: matchSchema(b)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	mb := &matcherBench{
		db:    db,
		sha:   make([]radixhash.Hash, n),
		blake: make([]radixhash.Hash, n),
		phash: make([]uint64, n),
		pdq:   make([]simhash.PDQHash, n),
	}
	const batchSize = 64
	for start := 0; start < n; start += batchSize {
		end := min(start+batchSize, n)
		batch := db.NewBatch(end - start)
		for i := start; i < end; i++ {
			mb.sha[i] = benchHash(uint64(i+1), 1)
			mb.blake[i] = benchHash(uint64(i+1), 2)
			// Pseudo-random pHash/PDQ so Hamming distances are realistic
			// (~uniform over 64/256 bits); otherwise every record matches.
			mb.phash[i] = splitmix64(uint64(i) + 1)
			for w := 0; w < 4; w++ {
				v := splitmix64(uint64(i)*4 + uint64(w) + 1)
				for b := 0; b < 8; b++ {
					mb.pdq[i][w*8+b] = byte(v >> (8 * uint(b)))
				}
			}
			entry := index.Entry{
				ValueID: schema.ValueID(i + 1),
				Hashes: []index.HashValue{
					{Namespace: 4, Hash: mb.sha[i]},
					{Namespace: 5, Hash: mb.blake[i]},
				},
				Sims: []index.SimValue{{Namespace: 3, Record: simhash.Record{PHash: mb.phash[i], PDQ: mb.pdq[i], Quality: 1}}},
			}
			if err := batch.Add(entry); err != nil {
				b.Fatal(err)
			}
		}
		if err := db.Apply(batch); err != nil {
			b.Fatal(err)
		}
	}
	mb.m, err = db.Matcher(index.MatcherSpec{SHA256: 4, BLAKE3: 5, Perceptual: 3})
	if err != nil {
		b.Fatal(err)
	}
	mb.shaIdx, _ = db.Hash(4)
	mb.blakeIdx, _ = db.Hash(5)
	mb.sim, _ = db.Sim(3)
	return mb
}

const matcherBenchN = 4096

func BenchmarkDirectSHA256Lookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]radixhash.ImageID, 0, 8)
	h := mb.sha[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = mb.shaIdx.Lookup(dst[:0], h)
	}
	_ = dst
}

func BenchmarkMatcherSHA256Lookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]index.Match, 0, 8)
	item := index.MatchItem{SHA256: &mb.sha[0]}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.MatchOne(dst[:0], item, index.MatchParams{})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func BenchmarkDirectBLAKE3Lookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]radixhash.ImageID, 0, 8)
	h := mb.blake[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = mb.blakeIdx.Lookup(dst[:0], h)
	}
	_ = dst
}

func BenchmarkMatcherBLAKE3Lookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]index.Match, 0, 8)
	item := index.MatchItem{BLAKE3: &mb.blake[0]}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.MatchOne(dst[:0], item, index.MatchParams{})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func exactBenchItems(mb *matcherBench, n int) []index.MatchItem {
	items := make([]index.MatchItem, n)
	for i := range items {
		h := mb.sha[i]
		items[i] = index.MatchItem{SHA256: &h}
	}
	return items
}

func BenchmarkDirectSHA256Batch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := exactBenchItems(mb, 64)
	dst := make([]radixhash.ImageID, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = dst[:0]
		for _, it := range items {
			dst = mb.shaIdx.Lookup(dst, *it.SHA256)
		}
	}
	_ = dst
}

func BenchmarkMatcherSHA256Batch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := exactBenchItems(mb, 64)
	dst := make([]index.Match, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.Match(dst[:0], items, index.MatchParams{})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func BenchmarkMatcherBLAKE3Batch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := make([]index.MatchItem, 64)
	for i := range items {
		h := mb.blake[i]
		items[i] = index.MatchItem{BLAKE3: &h}
	}
	dst := make([]index.Match, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.Match(dst[:0], items, index.MatchParams{})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func BenchmarkDirectBLAKE3Batch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := make([]index.MatchItem, 64)
	for i := range items {
		h := mb.blake[i]
		items[i] = index.MatchItem{BLAKE3: &h}
	}
	dst := make([]radixhash.ImageID, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = dst[:0]
		for _, it := range items {
			dst = mb.blakeIdx.Lookup(dst, *it.BLAKE3)
		}
	}
	_ = dst
}

func BenchmarkDirectPHashBatch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := perceptualBenchItems(mb, 64)
	dst := make([]simhash.Result, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = dst[:0]
		for _, it := range items {
			dst = mb.sim.SearchPHash(dst, *it.PHash, 1)
		}
	}
	_ = dst
}

func BenchmarkDirectPDQBatch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := make([]index.MatchItem, 64)
	for i := range items {
		p := mb.pdq[i]
		items[i] = index.MatchItem{PDQ: &p}
	}
	dst := make([]simhash.Result, 0, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = dst[:0]
		for _, it := range items {
			dst = mb.sim.SearchPDQ(dst, it.PDQ, 8)
		}
	}
	_ = dst
}

func BenchmarkDirectPHashLookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]simhash.Result, 0, 8)
	q := mb.phash[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = mb.sim.SearchPHash(dst[:0], q, 1)
	}
	_ = dst
}

func BenchmarkMatcherPHashLookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]index.Match, 0, 8)
	q := mb.phash[0]
	item := index.MatchItem{PHash: &q}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.MatchOne(dst[:0], item, index.MatchParams{PHashDistance: 1})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func BenchmarkDirectPDQLookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]simhash.Result, 0, 8)
	q := mb.pdq[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = mb.sim.SearchPDQ(dst[:0], &q, 8)
	}
	_ = dst
}

func BenchmarkMatcherPDQLookup(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	dst := make([]index.Match, 0, 8)
	q := mb.pdq[0]
	item := index.MatchItem{PDQ: &q}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.MatchOne(dst[:0], item, index.MatchParams{PDQDistance: 8})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func perceptualBenchItems(mb *matcherBench, n int) []index.MatchItem {
	items := make([]index.MatchItem, n)
	for i := range items {
		p := mb.phash[i]
		items[i] = index.MatchItem{PHash: &p}
	}
	return items
}

func BenchmarkMatcherPHashBatch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := perceptualBenchItems(mb, 64)
	dst := make([]index.Match, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.Match(dst[:0], items, index.MatchParams{PHashDistance: 1})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func BenchmarkMatcherPDQBatch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := make([]index.MatchItem, 64)
	for i := range items {
		p := mb.pdq[i]
		items[i] = index.MatchItem{PDQ: &p}
	}
	dst := make([]index.Match, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.Match(dst[:0], items, index.MatchParams{PDQDistance: 8})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

func BenchmarkMatcherMixedBatch64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := make([]index.MatchItem, 64)
	for i := range items {
		s := mb.sha[i]
		bl := mb.blake[i]
		ph := mb.phash[i]
		pd := mb.pdq[i]
		items[i] = index.MatchItem{SHA256: &s, BLAKE3: &bl, PHash: &ph, PDQ: &pd}
	}
	dst := make([]index.Match, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.Match(dst[:0], items, index.MatchParams{PHashDistance: 1, PDQDistance: 8})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

// BenchmarkMatcherDuplicateHeavyWorkspace64 is the duplicate-heavy case with
// caller-owned workspace scratch.
func BenchmarkMatcherDuplicateHeavyWorkspace64(b *testing.B) {
	db, err := index.Create(b.TempDir(), index.Spec{Schema: matchSchema(b)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	shared := radixhash.Hash{0x55}
	batch := db.NewBatch(64)
	for i := 0; i < 64; i++ {
		if err := batch.Add(index.Entry{ValueID: schema.ValueID(i + 1), Hashes: []index.HashValue{{Namespace: 4, Hash: shared}}}); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Apply(batch); err != nil {
		b.Fatal(err)
	}
	m, err := db.Matcher(index.MatcherSpec{SHA256: 4})
	if err != nil {
		b.Fatal(err)
	}
	items := make([]index.MatchItem, 64)
	for i := range items {
		items[i] = index.MatchItem{SHA256: &shared}
	}
	dst := make([]index.Match, 0, 4096)
	ws := &index.MatchWorkspace{Exact: make([]radixhash.ImageID, 0, 64)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = m.MatchWithWorkspace(dst[:0], ws, items, index.MatchParams{})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

// BenchmarkMatcherMixedWorkspace64 is the mixed batch with workspace scratch.
func BenchmarkMatcherMixedWorkspace64(b *testing.B) {
	mb := newMatcherBench(b, matcherBenchN)
	items := make([]index.MatchItem, 64)
	for i := range items {
		s := mb.sha[i]
		bl := mb.blake[i]
		ph := mb.phash[i]
		pd := mb.pdq[i]
		items[i] = index.MatchItem{SHA256: &s, BLAKE3: &bl, PHash: &ph, PDQ: &pd}
	}
	dst := make([]index.Match, 0, 128)
	ws := &index.MatchWorkspace{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = mb.m.MatchWithWorkspace(dst[:0], ws, items, index.MatchParams{PHashDistance: 1, PDQDistance: 8})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}

// BenchmarkMatcherDuplicateHeavyBatch64 matches 64 incoming images that all
// share one SHA-256 hash already associated with 64 stored images.
func BenchmarkMatcherDuplicateHeavyBatch64(b *testing.B) {
	db, err := index.Create(b.TempDir(), index.Spec{Schema: matchSchema(b)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	shared := radixhash.Hash{0x55}
	batch := db.NewBatch(64)
	for i := 0; i < 64; i++ {
		if err := batch.Add(index.Entry{ValueID: schema.ValueID(i + 1), Hashes: []index.HashValue{{Namespace: 4, Hash: shared}}}); err != nil {
			b.Fatal(err)
		}
	}
	if err := db.Apply(batch); err != nil {
		b.Fatal(err)
	}
	m, err := db.Matcher(index.MatcherSpec{SHA256: 4})
	if err != nil {
		b.Fatal(err)
	}
	items := make([]index.MatchItem, 64)
	for i := range items {
		items[i] = index.MatchItem{SHA256: &shared}
	}
	dst := make([]index.Match, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = m.Match(dst[:0], items, index.MatchParams{})
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = dst
}
