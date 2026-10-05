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

func equalPHash(a, b []PHashMatch) bool {
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

func equalPDQ(a, b []PDQMatch) bool {
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

func equalCombined(a, b []CombinedMatch) bool {
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

func oraclePHash(records []Record, query uint64, maxDistance int) []PHashMatch {
	out := make([]PHashMatch, 0)
	for i := range records {
		if d := DistancePHash(query, records[i].PHash); d <= maxDistance {
			out = append(out, PHashMatch{ImageID: uint64(i + 1), Distance: d})
		}
	}
	return out
}

func oraclePDQ(records []Record, query PDQHash, maxDistance uint16) []PDQMatch {
	out := make([]PDQMatch, 0)
	for i := range records {
		if d := DistancePDQ(&query, &records[i].PDQ); d <= maxDistance {
			out = append(out, PDQMatch{ImageID: uint64(i + 1), Distance: d})
		}
	}
	return out
}

func oracleCombined(records []Record, phashQuery uint64, maxPHash int, pdqQuery PDQHash, maxPDQ uint16) []CombinedMatch {
	out := make([]CombinedMatch, 0)
	for i := range records {
		ph := DistancePHash(phashQuery, records[i].PHash)
		if ph > maxPHash {
			continue
		}
		d := DistancePDQ(&pdqQuery, &records[i].PDQ)
		if d > maxPDQ {
			continue
		}
		out = append(out, CombinedMatch{ImageID: uint64(i + 1), PHashDistance: ph, PDQDistance: d})
	}
	return out
}
