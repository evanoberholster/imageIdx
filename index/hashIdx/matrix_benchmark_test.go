package hashidx

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/bench"
)

// Benchmark-only matrix for hashIdx, per AGENTS/Bench.MD sections 11, 12, 15,
// 16, 17, 19, 20, 23, and 26. Every dataset comes from a seeded internal/bench
// generator, every name derives from stable labels only, and setup stays
// outside the timed region.
//
// SIMD comparison is not available in this package: hashIdx ships scalar
// popcount kernels only (see the non-goals in doc.go) and exposes no
// scalar/dispatched pair, so the perceptual cases measure the one shipped
// kernel.
//
// Size metrics are reported after the timed loop: the first b.Loop call resets
// the benchmark timer and clears user metrics, so a ReportIndexSize call
// before the loop would be silently dropped.

// Deterministic seed streams. The miss and add streams are disjoint from the
// indexed digest stream, so a miss digest is never a member of the index.
const (
	matrixDigestSeed = int64(0x1D1D_0000_0001)
	matrixMissSeed   = int64(0x1D1D_0000_0002)
	matrixAddSeed    = int64(0x1D1D_0000_0003)
	matrixPHashSeed  = int64(0x1D1D_0000_0004)
	matrixPDQSeed    = int64(0x1D1D_0000_0005)
	matrixQuerySeed  = int64(0x1D1D_0000_0006)

	// matrixWorkCount sizes each precomputed hot/cold workload; it is a power
	// of two so the timed loop cycles it with a mask.
	matrixWorkCount = 1024
	// matrixRadiusCap sizes radius destinations so realistic hit sets never
	// grow inside the timed loop.
	matrixRadiusCap = 1024
	// matrixAddBatch is the precomputed digest window the Add benchmark
	// cycles while ImageIDs keep advancing, so every add is a fresh pair.
	matrixAddBatch = 1 << 16
	// matrixRecoveryMaxTail caps the WAL tail a Recovery reopen must replay.
	matrixRecoveryMaxTail = 100_000
)

// matrixSink consumes benchmark results so the compiler cannot eliminate the
// measured work.
var matrixSink uint64

// matrixSizing captures the resident and on-disk measurement taken once around
// index construction. Every leaf case reports its own copy so the metric lands
// on a benchmark line instead of a childless parent.
type matrixSizing struct {
	label  string
	before uint64
	after  uint64
	disk   uint64
	ids    int
}

// report emits the sizing log line and bytes-per-item metrics for one leaf.
func (s matrixSizing) report(b *testing.B) {
	b.Helper()
	bench.ReportIndexSize(b, s.label, s.before, s.after, s.disk, s.ids)
}

// matrixAlgorithms returns both digest algorithms in a fixed order.
func matrixAlgorithms() []Algorithm { return []Algorithm{AlgorithmSHA256, AlgorithmBLAKE3} }

// matrixCryptoEntries derives n digests from one deterministic stream;
// hashes[i] belongs to ImageID i+1.
func matrixCryptoEntries(seed int64, n int) ([]ExactEntry, []Hash) {
	raw := bench.Hash32s(seed, n)
	hashes := make([]Hash, n)
	entries := make([]ExactEntry, n)
	for i := range raw {
		hashes[i] = Hash(raw[i])
		entries[i] = ExactEntry{Hash: hashes[i], ImageID: ImageID(i + 1)}
	}
	return entries, hashes
}

// matrixMissHash returns a deterministic digest from a stream disjoint from
// every indexed digest set.
func matrixMissHash() Hash {
	return Hash(bench.Hash32s(matrixMissSeed, 1)[0])
}

// matrixPerceptualEntries derives n perceptual records from seeded pHash64 and
// PDQ256 streams.
func matrixPerceptualEntries(n int) []Entry {
	phashes := bench.PHashes(matrixPHashSeed, n)
	pdqs := bench.PDQHashes(matrixPDQSeed, n)
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{
			ImageID: ImageID(i + 1),
			PHash64: phashes[i],
			PDQ:     PDQHash(pdqs[i]),
		}
	}
	return entries
}

// matrixNewInMemoryExact builds a WAL-free exact index with n entries and
// captures resident heap growth. This is the read-path constructor: blocks are
// ordinary heap memory and no files are touched.
func matrixNewInMemoryExact(b *testing.B, algo Algorithm, n int) (*ExactIndex, []Hash, matrixSizing) {
	b.Helper()
	entries, hashes := matrixCryptoEntries(matrixDigestSeed, n)
	idx, sizing := matrixBuildInMemoryExact(b, algo, entries, fmt.Sprintf("HashExact/%s/N=%d", algo, n))
	return idx, hashes, sizing
}

// matrixBuildInMemoryExact constructs one in-memory exact index and captures
// the heap delta around construction. The entries slice stays live across both
// samples, so fixture bytes cancel out of the reported resident growth.
func matrixBuildInMemoryExact(b *testing.B, algo Algorithm, entries []ExactEntry, label string) (*ExactIndex, matrixSizing) {
	b.Helper()
	before := bench.HeapBytes()
	idx, err := newExactIndex(ExactSpec{Algorithm: algo, Entries: entries}, ExactConfig{})
	if err != nil {
		b.Fatal(err)
	}
	after := bench.HeapBytes()
	runtime.KeepAlive(entries)
	b.Cleanup(func() { _ = idx.Close() })
	return idx, matrixSizing{label: label, before: before, after: after, ids: len(entries)}
}

// matrixBuildPersistentExact builds a persistent index with n base entries
// plus an optional WAL-only tail (never checkpointed, so a later open must
// replay it), captures resident heap and on-disk bytes, and returns the
// still-open index plus its directory. Callers close the index before timing
// an open.
func matrixBuildPersistentExact(b *testing.B, algo Algorithm, n, tail int) (*ExactIndex, string, []Hash, matrixSizing) {
	b.Helper()
	dir := b.TempDir()
	entries, hashes := matrixCryptoEntries(matrixDigestSeed, n)
	before := bench.HeapBytes()
	idx, err := newExact(dir, ExactSpec{Algorithm: algo, Entries: entries}, ExactConfig{}, DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	if tail > 0 {
		tailEntries, _ := matrixCryptoEntries(matrixDigestSeed+1, tail)
		chunkSize := int(DefaultMaxBatchEntries)
		for start := 0; start < len(tailEntries); start += chunkSize {
			chunk := tailEntries[start:min(start+chunkSize, len(tailEntries))]
			for i := range chunk {
				chunk[i].ImageID = ImageID(n + start + i + 1)
			}
			if err := idx.Apply(Mutation{Add: chunk}); err != nil {
				_ = idx.Close()
				b.Fatal(err)
			}
		}
	}
	after := bench.HeapBytes()
	runtime.KeepAlive(entries)
	sizing := matrixSizing{
		label:  fmt.Sprintf("HashExact/%s/N=%d", algo, n),
		before: before,
		after:  after,
		disk:   bench.DiskBytes(dir),
		ids:    n,
	}
	return idx, dir, hashes, sizing
}

// matrixRecoveryTail is a deterministic WAL tail proportional to the base
// size, capped so large tiers stay practical.
func matrixRecoveryTail(n int) int {
	tail := n / 100
	if tail < 1 {
		tail = 1
	}
	if tail > matrixRecoveryMaxTail {
		tail = matrixRecoveryMaxTail
	}
	return tail
}

// matrixNewPerceptual builds a persistent perceptual index with n entries and
// captures resident heap plus on-disk bytes.
func matrixNewPerceptual(b *testing.B, n int) (*Index, []Entry, matrixSizing) {
	b.Helper()
	entries := matrixPerceptualEntries(n)
	idx, sizing := matrixNewPerceptualFrom(b, entries)
	return idx, entries, sizing
}

// matrixNewPerceptualFrom is matrixNewPerceptual with caller-supplied entries
// for adversarial cases. Vacuum repacks the applied records so the reported
// disk size is the index generation, not the ingest WAL.
func matrixNewPerceptualFrom(b *testing.B, entries []Entry) (*Index, matrixSizing) {
	b.Helper()
	dir := b.TempDir()
	before := bench.HeapBytes()
	idx, err := newIndex(dir, testPerceptualSpec(Config{}), DurabilityOS)
	if err != nil {
		b.Fatal(err)
	}
	chunkSize := int(DefaultMaxApplyEntries)
	for start := 0; start < len(entries); start += chunkSize {
		chunk := entries[start:min(start+chunkSize, len(entries))]
		if err := idx.Apply(chunk); err != nil {
			_ = idx.Close()
			b.Fatal(err)
		}
	}
	if len(entries) > 0 {
		if err := idx.Vacuum(nil); err != nil {
			_ = idx.Close()
			b.Fatal(err)
		}
	}
	after := bench.HeapBytes()
	runtime.KeepAlive(entries)
	sizing := matrixSizing{
		label:  fmt.Sprintf("Perceptual/N=%d", len(entries)),
		before: before,
		after:  after,
		disk:   bench.DiskBytes(dir),
		ids:    len(entries),
	}
	b.Cleanup(func() { _ = idx.Close() })
	return idx, sizing
}

// matrixReportScanRate reports records examined per second after the timed
// loop.
func matrixReportScanRate(b *testing.B, idx *Index) {
	b.Helper()
	if n := idx.Metadata().RecordCount; n > 0 {
		b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "hashes/sec")
	}
}

// matrixPHashRadius runs one pHash64 radius case with a nil candidate set.
func matrixPHashRadius(b *testing.B, idx *Index, sizing matrixSizing, name string, query uint64, maxDistance uint8) {
	b.Helper()
	b.Run("Case="+name, func(b *testing.B) {
		dst := make([]Result, 0, matrixRadiusCap)
		b.ReportAllocs()
		for b.Loop() {
			dst = idx.SearchPHashRadius(dst[:0], query, maxDistance, nil)
			matrixSink += uint64(len(dst))
		}
		sizing.report(b)
		matrixReportScanRate(b, idx)
	})
}

// matrixPDQRadius runs one PDQ256 radius case with a nil candidate set.
func matrixPDQRadius(b *testing.B, idx *Index, sizing matrixSizing, name string, query PDQHash, maxDistance uint16) {
	b.Helper()
	b.Run("Case="+name, func(b *testing.B) {
		dst := make([]Result, 0, matrixRadiusCap)
		b.ReportAllocs()
		for b.Loop() {
			dst = idx.SearchPDQRadius(dst[:0], query, maxDistance, nil)
			matrixSink += uint64(len(dst))
		}
		sizing.report(b)
		matrixReportScanRate(b, idx)
	})
}

// matrixPHashTopK runs one pHash64 Top-K case. The exported zero-value
// Workspace is reused across calls, matching intended caller usage.
func matrixPHashTopK(b *testing.B, idx *Index, sizing matrixSizing, query uint64, k int) {
	b.Helper()
	b.Run(fmt.Sprintf("Case=TopK%d", k), func(b *testing.B) {
		ws := &Workspace{}
		dst := make([]Result, 0, k)
		_ = idx.SearchPHashTopK(dst, query, k, nil, ws)
		b.ReportAllocs()
		for b.Loop() {
			dst = idx.SearchPHashTopK(dst[:0], query, k, nil, ws)
			matrixSink += uint64(len(dst))
		}
		sizing.report(b)
		matrixReportScanRate(b, idx)
	})
}

// matrixPDQTopK runs one PDQ256 Top-K case.
func matrixPDQTopK(b *testing.B, idx *Index, sizing matrixSizing, query PDQHash, k int) {
	b.Helper()
	b.Run(fmt.Sprintf("Case=TopK%d", k), func(b *testing.B) {
		ws := &Workspace{}
		dst := make([]Result, 0, k)
		_ = idx.SearchPDQTopK(dst, query, k, nil, ws)
		b.ReportAllocs()
		for b.Loop() {
			dst = idx.SearchPDQTopK(dst[:0], query, k, nil, ws)
			matrixSink += uint64(len(dst))
		}
		sizing.report(b)
		matrixReportScanRate(b, idx)
	})
}

// BenchmarkHashExact is the cryptographic digest matrix: point lookup and
// membership by algorithm and size, per-iteration writes, clean opens, WAL
// recovery, and hostile ends.
func BenchmarkHashExact(b *testing.B) {
	b.Run("Lookup", benchmarkHashExactLookup)
	b.Run("Contains", benchmarkHashExactContains)
	b.Run("Add", benchmarkHashExactAdd)
	b.Run("Open", benchmarkHashExactOpen)
	b.Run("Recovery", benchmarkHashExactRecovery)
	b.Run("Adversarial", benchmarkHashExactAdversarial)
}

func benchmarkHashExactLookup(b *testing.B) {
	for _, algo := range matrixAlgorithms() {
		b.Run(fmt.Sprintf("Algo=%s", algo), func(b *testing.B) {
			for _, n := range bench.Sizes(b) {
				b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
					idx, hashes, sizing := matrixNewInMemoryExact(b, algo, n)
					hit := hashes[n/2]
					miss := matrixMissHash()
					hot := bench.HotSetWorkload(matrixQuerySeed, n, matrixWorkCount, 0.2, 0.9, nil)
					cold := bench.HotSetWorkload(matrixQuerySeed+1, n, matrixWorkCount, 0.2, 0.0, nil)

					b.Run("Case=Hit", func(b *testing.B) {
						dst := make([]ImageID, 0, n)
						b.ReportAllocs()
						for b.Loop() {
							dst = idx.Lookup(dst[:0], hit)
							matrixSink += uint64(len(dst))
						}
						sizing.report(b)
					})
					b.Run("Case=Miss", func(b *testing.B) {
						dst := make([]ImageID, 0, n)
						b.ReportAllocs()
						for b.Loop() {
							dst = idx.Lookup(dst[:0], miss)
							matrixSink += uint64(len(dst))
						}
						sizing.report(b)
					})
					b.Run("Case=Hot", func(b *testing.B) {
						dst := make([]ImageID, 0, n)
						next := 0
						b.ReportAllocs()
						for b.Loop() {
							dst = idx.Lookup(dst[:0], hashes[hot[next]])
							next = (next + 1) & (matrixWorkCount - 1)
							matrixSink += uint64(len(dst))
						}
						sizing.report(b)
					})
					b.Run("Case=Cold", func(b *testing.B) {
						dst := make([]ImageID, 0, n)
						next := 0
						b.ReportAllocs()
						for b.Loop() {
							dst = idx.Lookup(dst[:0], hashes[cold[next]])
							next = (next + 1) & (matrixWorkCount - 1)
							matrixSink += uint64(len(dst))
						}
						sizing.report(b)
					})
				})
			}
		})
	}
}

func benchmarkHashExactContains(b *testing.B) {
	for _, algo := range matrixAlgorithms() {
		b.Run(fmt.Sprintf("Algo=%s", algo), func(b *testing.B) {
			for _, n := range bench.Sizes(b) {
				b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
					idx, hashes, sizing := matrixNewInMemoryExact(b, algo, n)
					hit := hashes[n/2]
					miss := matrixMissHash()
					hot := bench.HotSetWorkload(matrixQuerySeed, n, matrixWorkCount, 0.2, 0.9, nil)
					cold := bench.HotSetWorkload(matrixQuerySeed+1, n, matrixWorkCount, 0.2, 0.0, nil)

					b.Run("Case=Hit", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							if idx.Contains(hit) {
								matrixSink++
							}
						}
						sizing.report(b)
					})
					b.Run("Case=Miss", func(b *testing.B) {
						b.ReportAllocs()
						for b.Loop() {
							if idx.Contains(miss) {
								matrixSink++
							}
						}
						sizing.report(b)
					})
					b.Run("Case=Hot", func(b *testing.B) {
						next := 0
						b.ReportAllocs()
						for b.Loop() {
							if idx.Contains(hashes[hot[next]]) {
								matrixSink++
							}
							next = (next + 1) & (matrixWorkCount - 1)
						}
						sizing.report(b)
					})
					b.Run("Case=Cold", func(b *testing.B) {
						next := 0
						b.ReportAllocs()
						for b.Loop() {
							if idx.Contains(hashes[cold[next]]) {
								matrixSink++
							}
							next = (next + 1) & (matrixWorkCount - 1)
						}
						sizing.report(b)
					})
				})
			}
		})
	}
}

// benchmarkHashExactAdd measures single-pair writes into an index of size N.
// The index grows during the run by design: fresh (Hash, ImageID) pairs keep
// every add on the real WAL + overlay path.
func benchmarkHashExactAdd(b *testing.B) {
	for _, algo := range matrixAlgorithms() {
		b.Run(fmt.Sprintf("Algo=%s", algo), func(b *testing.B) {
			for _, n := range bench.Sizes(b) {
				b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
					idx, _, _, sizing := matrixBuildPersistentExact(b, algo, n, 0)
					b.Cleanup(func() { _ = idx.Close() })
					adds := bench.Hash32s(matrixAddSeed, matrixAddBatch)
					nextID := n + 1
					slot := 0
					b.ReportAllocs()
					for b.Loop() {
						if err := idx.Add(Hash(adds[slot]), ImageID(nextID)); err != nil {
							b.Fatal(err)
						}
						slot = (slot + 1) & (matrixAddBatch - 1)
						nextID++
					}
					sizing.report(b)
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "adds/sec")
				})
			}
		})
	}
}

// benchmarkHashExactOpen measures clean reopen of a fully checkpointed index
// (no WAL tail). Each iteration closes the reopened handle, so per-iteration
// file descriptors stay bounded; the close is part of the measurement.
func benchmarkHashExactOpen(b *testing.B) {
	for _, algo := range matrixAlgorithms() {
		b.Run(fmt.Sprintf("Algo=%s", algo), func(b *testing.B) {
			for _, n := range bench.Sizes(b) {
				b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
					idx, dir, _, sizing := matrixBuildPersistentExact(b, algo, n, 0)
					if err := idx.Close(); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					for b.Loop() {
						opened, err := openExact(dir, ExactConfig{}, DurabilityOS)
						if err != nil {
							b.Fatal(err)
						}
						matrixSink += opened.LiveCount()
						if err := opened.Close(); err != nil {
							b.Fatal(err)
						}
					}
					sizing.report(b)
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "opens/sec")
				})
			}
		})
	}
}

// benchmarkHashExactRecovery measures reopen plus WAL replay of a tail that
// was never checkpointed. The tail is proportional to N (capped), so
// recovery cost is visible as the dataset scales.
func benchmarkHashExactRecovery(b *testing.B) {
	for _, algo := range matrixAlgorithms() {
		b.Run(fmt.Sprintf("Algo=%s", algo), func(b *testing.B) {
			for _, n := range bench.Sizes(b) {
				b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
					idx, dir, _, sizing := matrixBuildPersistentExact(b, algo, n, matrixRecoveryTail(n))
					if err := idx.Close(); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					for b.Loop() {
						opened, err := openExact(dir, ExactConfig{}, DurabilityOS)
						if err != nil {
							b.Fatal(err)
						}
						matrixSink += opened.LiveCount()
						if err := opened.Close(); err != nil {
							b.Fatal(err)
						}
					}
					sizing.report(b)
					b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "recoveries/sec")
				})
			}
		})
	}
}

// benchmarkHashExactAdversarial covers section-19 hostile ends: an empty
// index, the zero and all-ones digests, and an unindexed digest.
func benchmarkHashExactAdversarial(b *testing.B) {
	var zeros, ones Hash
	for i := range ones {
		ones[i] = 0xFF
	}
	miss := matrixMissHash()
	for _, algo := range matrixAlgorithms() {
		b.Run(fmt.Sprintf("Algo=%s", algo), func(b *testing.B) {
			b.Run("Case=Empty", func(b *testing.B) {
				idx, sizing := matrixBuildInMemoryExact(b, algo, nil, fmt.Sprintf("HashExact/%s/empty", algo))
				dst := make([]ImageID, 0, 4)
				b.ReportAllocs()
				for b.Loop() {
					dst = idx.Lookup(dst[:0], miss)
					matrixSink += uint64(len(dst))
				}
				sizing.report(b)
			})
			idx, sizing := matrixBuildInMemoryExact(b, algo, []ExactEntry{
				{Hash: zeros, ImageID: 1},
				{Hash: ones, ImageID: 2},
			}, fmt.Sprintf("HashExact/%s/adversarial", algo))
			for _, tc := range []struct {
				name string
				hash Hash
			}{
				{"ZeroDigest", zeros},
				{"AllOnesDigest", ones},
				{"UnindexedDigest", miss},
			} {
				b.Run("Case="+tc.name, func(b *testing.B) {
					dst := make([]ImageID, 0, 2)
					b.ReportAllocs()
					for b.Loop() {
						dst = idx.Lookup(dst[:0], tc.hash)
						matrixSink += uint64(len(dst))
					}
					sizing.report(b)
				})
			}
		})
	}
}

// BenchmarkPerceptual is the perceptual-hash matrix: pHash64 and PDQ256 radius
// scans at controlled Hamming distances plus Top-K, per size.
func BenchmarkPerceptual(b *testing.B) {
	for _, kind := range []string{"PHash64", "PDQ256"} {
		b.Run(kind, func(b *testing.B) {
			for _, n := range bench.Sizes(b) {
				b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
					benchmarkPerceptualSize(b, kind, n)
				})
			}
		})
	}
	b.Run("Adversarial", benchmarkPerceptualAdversarial)
}

func benchmarkPerceptualSize(b *testing.B, kind string, n int) {
	idx, entries, sizing := matrixNewPerceptual(b, n)
	rng := bench.NewRNG(matrixQuerySeed)
	if kind == "PHash64" {
		base := entries[n/2].PHash64
		matrixPHashRadius(b, idx, sizing, "Exact", base, 0)
		matrixPHashRadius(b, idx, sizing, "Small", bench.Hamming64(rng, base, 4), 4)
		matrixPHashRadius(b, idx, sizing, "Medium", bench.Hamming64(rng, base, 16), 16)
		// The no-match query sits 32 bits from the indexed base while the
		// threshold stays at 4, so the indexed set yields no match.
		matrixPHashRadius(b, idx, sizing, "NoMatch", bench.Hamming64(rng, base, 32), 4)
		matrixPHashTopK(b, idx, sizing, base, 10)
		matrixPHashTopK(b, idx, sizing, base, 100)
		return
	}
	base := entries[n/2].PDQ
	matrixPDQRadius(b, idx, sizing, "Exact", base, 0)
	matrixPDQRadius(b, idx, sizing, "Small", PDQHash(bench.HammingPDQ(rng, base, 8)), 8)
	matrixPDQRadius(b, idx, sizing, "Medium", PDQHash(bench.HammingPDQ(rng, base, 32)), 32)
	// The no-match query sits 128 bits from the indexed base while the
	// threshold stays at 8.
	matrixPDQRadius(b, idx, sizing, "NoMatch", PDQHash(bench.HammingPDQ(rng, base, 128)), 8)
	matrixPDQTopK(b, idx, sizing, base, 10)
}

// benchmarkPerceptualAdversarial covers the empty index and a single entry,
// where every result either trivially matches or is absent.
func benchmarkPerceptualAdversarial(b *testing.B) {
	b.Run("Empty", func(b *testing.B) {
		idx, _, sizing := matrixNewPerceptual(b, 0)
		query := bench.PHashes(matrixQuerySeed, 1)[0]
		pdq := PDQHash(bench.PDQHashes(matrixQuerySeed, 1)[0])
		b.Run("PHash64", func(b *testing.B) {
			matrixPHashRadius(b, idx, sizing, "Radius", query, 4)
			matrixPHashTopK(b, idx, sizing, query, 10)
		})
		b.Run("PDQ256", func(b *testing.B) {
			matrixPDQRadius(b, idx, sizing, "Radius", pdq, 8)
			matrixPDQTopK(b, idx, sizing, pdq, 10)
		})
	})
	b.Run("Single", func(b *testing.B) {
		entries := matrixPerceptualEntries(1)
		idx, sizing := matrixNewPerceptualFrom(b, entries)
		b.Run("PHash64", func(b *testing.B) {
			base := entries[0].PHash64
			miss := bench.Hamming64(bench.NewRNG(matrixQuerySeed), base, 4)
			matrixPHashRadius(b, idx, sizing, "Exact", base, 0)
			matrixPHashRadius(b, idx, sizing, "Miss", miss, 0)
			matrixPHashTopK(b, idx, sizing, base, 10)
		})
		b.Run("PDQ256", func(b *testing.B) {
			base := entries[0].PDQ
			miss := PDQHash(bench.HammingPDQ(bench.NewRNG(matrixQuerySeed), base, 8))
			matrixPDQRadius(b, idx, sizing, "Exact", base, 0)
			matrixPDQRadius(b, idx, sizing, "Miss", miss, 0)
			matrixPDQTopK(b, idx, sizing, base, 10)
		})
	})
}
