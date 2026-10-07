package simhash

import (
	"math/rand"
	"testing"

	bench "github.com/evanoberholster/imageIdx/internal/bench"
)

// shardBytes returns a MaxShardBytes that fits exactly pages logical pages per
// shard for the given sequence block size.
func shardBytes(pages int, seqBlockRecords int) uint64 {
	g := newGeometry(seqBlockRecords)
	return uint64(headerBytes) + uint64(pages)*g.hashPageStride
}

func openStore(t testing.TB, cfg Config) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// randRecords returns n deterministic random canonical records.
func randRecords(seed int64, n int) []Record {
	words := bench.PDQHashes(seed, n)
	rng := bench.NewRNG(seed*6364136223846793005 + 1442695040888963407)
	out := make([]Record, n)
	for i := range out {
		out[i].PHash = rng.Uint64()
		*hashWords(&out[i].PDQ) = words[i]
		out[i].Quality = uint8(rng.Intn(256))
	}
	return out
}

func flipPDQ(rng *rand.Rand, h PDQHash, distance int) PDQHash {
	var out PDQHash
	*hashWords(&out) = bench.HammingPDQ(rng, *hashWords(&h), distance)
	return out
}

func flipPHash(rng *rand.Rand, h uint64, distance int) uint64 {
	return bench.Hamming64(rng, h, distance)
}

// equalResults reports whether two result slices are identical, including
// ImageID, Distance, and order.
func equalResults(a, b []Result) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// oraclePHash is the brute-force reference for SearchPHash. The exact distance
// is 0..64 and is compared before narrowing to uint8.
func oraclePHash(records []Record, query uint64, maxDistance uint8) []Result {
	out := make([]Result, 0)
	for i := range records {
		if d := DistancePHash(query, records[i].PHash); d <= maxDistance {
			out = append(out, Result{ImageID: uint64(i + 1), Distance: d})
		}
	}
	return out
}

// oraclePDQ is the brute-force reference for SearchPDQ. It computes the exact
// distance as uint16 (0..256), clamps the threshold to MaxPDQSearchDistance to
// mirror the public API, and only then emits; every emitted distance is <= 128
// and fits uint8 exactly.
func oraclePDQ(records []Record, query PDQHash, maxDistance uint8) []Result {
	limit := maxDistance
	if limit > MaxPDQSearchDistance {
		limit = MaxPDQSearchDistance
	}
	out := make([]Result, 0)
	for i := range records {
		d := DistancePDQ(&query, &records[i].PDQ)
		if d > uint16(limit) {
			continue
		}
		out = append(out, Result{ImageID: uint64(i + 1), Distance: uint8(d)})
	}
	return out
}
