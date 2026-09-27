package bench

import (
	"math/rand"
	"sort"
)

// QueryKind selects a deterministic access pattern over an indexed set of n
// items. The picker returns precomputed indexes so no random generation runs
// inside a measured loop.
type QueryKind uint8

const (
	// Sequential walks 0..n-1 in order.
	Sequential QueryKind = iota
	// Uniform samples uniformly at random.
	Uniform
	// Zipf samples a Zipf distribution, approximating natural popularity.
	Zipf
	// Clustered concentrates most queries around a few centers.
	Clustered
)

// String returns a stable benchstat label.
func (k QueryKind) String() string {
	switch k {
	case Sequential:
		return "Sequential"
	case Uniform:
		return "Uniform"
	case Zipf:
		return "Zipf"
	case Clustered:
		return "Clustered"
	default:
		return "Unknown"
	}
}

// QueryKinds returns the four kinds in a fixed order.
func QueryKinds() []QueryKind { return []QueryKind{Sequential, Uniform, Zipf, Clustered} }

// Workload returns count precomputed indexes in [0, n) following kind.
func Workload(seed int64, kind QueryKind, n, count int) []int {
	if n <= 0 || count <= 0 {
		return nil
	}
	rng := NewRNG(seed)
	out := make([]int, count)
	switch kind {
	case Sequential:
		for i := range out {
			out[i] = i % n
		}
	case Zipf:
		zipf := rand.NewZipf(rng, 1.2, 1, uint64(n-1))
		for i := range out {
			out[i] = int(zipf.Uint64())
		}
	case Clustered:
		centers := 64
		if n < centers {
			centers = n
		}
		position := make([]int, centers)
		for i := range position {
			position[i] = rng.Intn(n)
		}
		spread := n / 100
		if spread < 1 {
			spread = 1
		}
		for i := range out {
			if rng.Intn(10) == 0 {
				out[i] = rng.Intn(n)
				continue
			}
			center := position[rng.Intn(centers)]
			offset := rng.Intn(2*spread+1) - spread
			idx := center + offset
			if idx < 0 {
				idx += n
			}
			out[i] = idx % n
		}
	default: // Uniform
		for i := range out {
			out[i] = rng.Intn(n)
		}
	}
	return out
}

// HotSetWorkload returns count indexes where hotProb of the queries hit a
// hot set of hotFraction of the items, approximating repeated searches over
// common people, albums, and dates. hotIDs is the ascending set of hot
// indexes; it is built deterministically from seed when nil.
func HotSetWorkload(seed int64, n, count int, hotFraction, hotProb float64, hotIDs []int) []int {
	if n <= 0 || count <= 0 {
		return nil
	}
	rng := NewRNG(seed)
	if hotIDs == nil {
		hotSize := int(float64(n) * hotFraction)
		if hotSize < 1 {
			hotSize = 1
		}
		if hotSize > n {
			hotSize = n
		}
		hotIDs = make([]int, 0, hotSize)
		seen := make(map[int]struct{}, hotSize)
		for len(hotIDs) < hotSize {
			id := rng.Intn(n)
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			hotIDs = append(hotIDs, id)
		}
		sort.Ints(hotIDs)
	}
	out := make([]int, count)
	for i := range out {
		if rng.Float64() < hotProb {
			out[i] = hotIDs[rng.Intn(len(hotIDs))]
			continue
		}
		out[i] = rng.Intn(n)
	}
	return out
}
