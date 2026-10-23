package bench

import (
	"math/rand"
	"os"
	"slices"
	"sort"
	"testing"
)

// largeEnv enables the 10M tier when set to a non-empty value. Large tiers
// are explicit so a plain `go test -bench=.` run cannot allocate tens of
// gigabytes by surprise.
const largeEnv = "IMAGEIDX_BENCH_LARGE"

// SmallSizes is the always-safe tier list for benchmarks that also run with
// -short.
var smallSizes = []int{100, 1_000, 10_000}

// defaultSizes is the standard matrix from AGENTS/CONTRACTS.md#benchmarks.
var defaultSizes = []int{1_000, 10_000, 100_000, 1_000_000}

// largeSizes extends defaultSizes with the explicit 10M tier.
var largeSizes = []int{1_000, 10_000, 100_000, 1_000_000, 10_000_000}

// Sizes returns the dataset sizes for a benchmark. Values above 100K are
// dropped under testing.Short, and the 10M tier requires
// IMAGEIDX_BENCH_LARGE=1. The returned slice is a copy.
func Sizes(tb testing.TB) []int {
	tb.Helper()
	var sizes []int
	switch {
	case testing.Short() || tb == nil:
		sizes = smallSizes
	case os.Getenv(largeEnv) != "":
		sizes = largeSizes
	default:
		sizes = defaultSizes
	}
	return append([]int(nil), sizes...)
}

// NewRNG returns a deterministic PRNG. math/rand's NewSource stream is
// stable across Go releases, which is what benchmark reproducibility needs.
func NewRNG(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

// DenseIDs returns the dense ID sequence 1..n. ImageID zero is reserved and
// never generated.
func DenseIDs(n int) []uint64 {
	if n < 0 {
		n = 0
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(i + 1)
	}
	return out
}

// SparseIDs returns n distinct ascending IDs drawn from [1, max], a
// realistic sparse uint64 distribution. max must be at least n; max is
// clamped to 1<<48-1, the bitmap key domain.
func SparseIDs(n int, max uint64, seed int64) []uint64 {
	if n < 0 {
		n = 0
	}
	if max < uint64(n)+1 {
		max = uint64(n) + 1
	}
	if max > 1<<48-1 {
		max = 1<<48 - 1
	}
	rng := NewRNG(seed)
	out := make([]uint64, 0, n)
	for len(out) < n {
		out = out[:0]
		for len(out) < n {
			out = append(out, rng.Uint64()%max+1)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		out = slices.Compact(out)
	}
	return out
}

// Density names a bitmap container density tier. roar stores values up to
// 4096 as an array container and denser containers as bitmaps, so the tiers
// straddle that boundary.
type Density uint8

const (
	// VerySparse targets ~16 of 65,536 values per container (array).
	VerySparse Density = iota
	// Sparse targets ~256 values per container (array).
	Sparse
	// Medium targets ~4,096 values per container (the array/bitmap edge).
	Medium
	// Dense targets ~32,768 values per container (bitmap).
	Dense
)

// String returns a stable benchstat label.
func (d Density) String() string {
	switch d {
	case VerySparse:
		return "VerySparse"
	case Sparse:
		return "Sparse"
	case Medium:
		return "Medium"
	case Dense:
		return "Dense"
	default:
		return "Unknown"
	}
}

// Densities returns every density tier in ascending order.
func Densities() []Density { return []Density{VerySparse, Sparse, Medium, Dense} }

// perContainer is the target cardinality of one 65,536-value container per
// density tier.
func (d Density) perContainer() int {
	switch d {
	case VerySparse:
		return 16
	case Sparse:
		return 256
	case Medium:
		return 4096
	default:
		return 32768
	}
}

// BitmapIDs returns n ascending IDs concentrated into containers of the
// requested density. Every returned ID is in [1, 1<<48). Deterministic for
// a given seed, count, n, and density.
func BitmapIDs(seed int64, n int, d Density) []uint64 {
	if n < 0 {
		n = 0
	}
	per := d.perContainer()
	containers := (n + per - 1) / per
	if containers == 0 {
		return nil
	}
	rng := NewRNG(seed)
	seen := make(map[uint64]struct{}, n)
	out := make([]uint64, 0, n)
	for c := 0; c < containers && len(out) < n; c++ {
		remaining := n - len(out)
		want := per
		if remaining < want {
			want = remaining
		}
		if want > 65536 {
			want = 65536
		}
		used := make(map[uint16]struct{}, want)
		key := uint64(c+1) << 16
		for len(used) < want {
			v := uint16(rng.Intn(1 << 16))
			if _, ok := used[v]; ok {
				continue
			}
			used[v] = struct{}{}
			id := key | uint64(v)
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// BitmapSets returns count independently generated ID sets of cardinality n
// at the requested density. The sets overlap by construction (each spans the
// same container keys), which is what makes intersection benchmarks
// meaningful.
func BitmapSets(seed int64, count, n int, d Density) [][]uint64 {
	out := make([][]uint64, count)
	for k := range out {
		out[k] = BitmapIDs(seed+int64(k)*7919, n, d)
	}
	return out
}

// TimestampDistribution names a timestamp access distribution.
type TimestampDistribution uint8

const (
	// TimeUniform spreads timestamps evenly over a wide span.
	TimeUniform TimestampDistribution = iota
	// TimeClustered concentrates timestamps around a few centers.
	TimeClustered
	// TimeBursty emits dense bursts separated by quiet gaps.
	TimeBursty
	// TimeRecent concentrates most timestamps near the top of the domain.
	TimeRecent
	// TimeMultiYear spreads over roughly a decade at one-second resolution.
	TimeMultiYear
)

// String returns a stable benchstat label.
func (d TimestampDistribution) String() string {
	switch d {
	case TimeUniform:
		return "Uniform"
	case TimeClustered:
		return "Clustered"
	case TimeBursty:
		return "Bursty"
	case TimeRecent:
		return "Recent"
	case TimeMultiYear:
		return "MultiYear"
	default:
		return "Unknown"
	}
}

// TimestampDistributions returns every distribution in a fixed order.
func TimestampDistributions() []TimestampDistribution {
	return []TimestampDistribution{TimeUniform, TimeClustered, TimeBursty, TimeRecent, TimeMultiYear}
}

const (
	// timeSpan is a representative ~10-year window at one-second resolution.
	timeSpan = uint64(10 * 365 * 24 * 3600)
	// timeBase keeps timestamps comfortably inside the uint64 domain.
	timeBase = uint64(1) << 40
)

// Timestamps returns n deterministic raw timestamps for the requested
// distribution. Order is not significant: callers pass them to an index
// constructor, which sorts internally.
func Timestamps(seed int64, n int, d TimestampDistribution) []uint64 {
	if n < 0 {
		n = 0
	}
	rng := NewRNG(seed)
	out := make([]uint64, n)
	switch d {
	case TimeClustered:
		centers := make([]uint64, 64)
		for i := range centers {
			centers[i] = timeBase + rng.Uint64()%timeSpan
		}
		for i := range out {
			center := centers[rng.Intn(len(centers))]
			out[i] = center + uint64(rng.Intn(3600))
		}
	case TimeBursty:
		burst := timeBase
		for i := range out {
			if i%512 == 0 {
				burst += uint64(rng.Intn(24*3600)) + 3600
			}
			out[i] = burst + uint64(i%512)
		}
	case TimeRecent:
		for i := range out {
			// 90% within the last ~2% of the span, 10% anywhere.
			if rng.Intn(10) == 0 {
				out[i] = timeBase + rng.Uint64()%timeSpan
				continue
			}
			out[i] = timeBase + timeSpan - uint64(rng.Intn(int(timeSpan/50)+1))
		}
	case TimeMultiYear:
		for i := range out {
			out[i] = timeBase + rng.Uint64()%timeSpan
		}
	default: // TimeUniform
		step := timeSpan / uint64(maxInt(n, 1))
		for i := range out {
			out[i] = timeBase + uint64(i)*step + uint64(rng.Intn(int(max64(step, 1))))
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// Hamming64 returns base with exactly distance bits flipped. distance is
// clamped to [0,64]; the flipped bit positions are distinct and
// deterministic for the given RNG.
func Hamming64(rng *rand.Rand, base uint64, distance int) uint64 {
	if distance <= 0 {
		return base
	}
	if distance > 64 {
		distance = 64
	}
	order := rng.Perm(64)
	out := base
	for _, bit := range order[:distance] {
		out ^= 1 << uint(bit)
	}
	return out
}

// HammingPDQ returns base with exactly distance bits flipped across the 256
// PDQ bits. distance is clamped to [0,256].
func HammingPDQ(rng *rand.Rand, base [4]uint64, distance int) [4]uint64 {
	if distance <= 0 {
		return base
	}
	if distance > 256 {
		distance = 256
	}
	order := rng.Perm(256)
	out := base
	for _, bit := range order[:distance] {
		out[bit/64] ^= 1 << uint(bit%64)
	}
	return out
}

// PHashes returns n deterministic pHash64 values.
func PHashes(seed int64, n int) []uint64 {
	rng := NewRNG(seed)
	out := make([]uint64, n)
	for i := range out {
		out[i] = rng.Uint64()
	}
	return out
}

// PDQHashes returns n deterministic PDQ hashes.
func PDQHashes(seed int64, n int) [][4]uint64 {
	rng := NewRNG(seed)
	out := make([][4]uint64, n)
	for i := range out {
		out[i] = [4]uint64{rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()}
	}
	return out
}

// Hash32s returns n deterministic 32-byte digests.
func Hash32s(seed int64, n int) [][32]byte {
	rng := NewRNG(seed)
	out := make([][32]byte, n)
	for i := range out {
		for w := 0; w < 4; w++ {
			v := rng.Uint64()
			for b := 0; b < 8; b++ {
				out[i][w*8+b] = byte(v >> (8 * b))
			}
		}
	}
	return out
}
