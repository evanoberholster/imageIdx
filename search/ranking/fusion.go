package ranking

import (
	"slices"

	searchmetrics "github.com/evanoberholster/imageIdx/search/metrics"
)

// Fusion combines heterogeneous signal lanes into one final Rank per
// candidate. Implementations write dst[i] aligned with src.ImageIDs[i] and
// never reorder ImageIDs; sorting happens at the output stage.
type Fusion interface {
	Fuse(dst []Rank, src *Signals) error
}

// DefaultRRFK is the standard RRF constant damping deep positions.
const DefaultRRFK = 60.0

// RRF implements Reciprocal Rank Fusion over per-signal orderings. For each
// lane, candidates sort by (signal descending, ImageID ascending); position
// p (1-indexed) contributes 1/(K+p). Missing (NoRank) lanes contribute
// nothing. Scores scale to uint16 higher-is-better: theoretical max
// S/(K+1) maps to 65535, zero score maps to NoRank.
//
// RRF removes dependence on incompatible numeric scales at the cost of
// sorting once per lane: O(S*N log N). It is deterministic; equal fused
// scores map to equal Ranks, with final tie-breaking by ImageID at sort
// time. Buffers reuse across calls; one RRF value must not be shared by
// concurrent goroutines.
type RRF struct {
	// K damps deep positions. Non-positive means DefaultRRFK.
	K      float64
	scores []float64
	perm   []int
}

// Fuse implements Fusion. dst must hold at least len(src.ImageIDs) slots.
func (f *RRF) Fuse(dst []Rank, src *Signals) error {
	return f.fuse(dst, src, nil)
}

func (f *RRF) fuse(dst []Rank, src *Signals, m *searchmetrics.RankMetrics) error {
	if err := src.Validate(); err != nil {
		return err
	}
	n := len(src.ImageIDs)
	if len(dst) < n {
		return ErrShortBuffer
	}
	if n == 0 {
		return nil
	}
	s := int(src.Count)
	if s == 0 {
		for i := 0; i < n; i++ {
			dst[i] = NoRank
		}
		if m != nil {
			m.Candidates += uint64(n)
			m.Skipped += uint64(n)
		}
		return nil
	}
	k := f.K
	if k <= 0 {
		k = DefaultRRFK
	}
	if cap(f.scores) < n {
		f.scores = make([]float64, n)
	} else {
		f.scores = f.scores[:n]
		clear(f.scores)
	}
	if cap(f.perm) < n {
		f.perm = make([]int, n)
	} else {
		f.perm = f.perm[:n]
	}

	for lane := 0; lane < s; lane++ {
		vals := src.Values[lane]
		ids := src.ImageIDs
		for i := range f.perm {
			f.perm[i] = i
		}
		slices.SortFunc(f.perm, func(a, b int) int {
			va, vb := vals[a], vals[b]
			if va != vb {
				if va < vb {
					return 1 // descending: larger first
				}
				return -1
			}
			if ids[a] < ids[b] {
				return -1
			}
			if ids[a] > ids[b] {
				return 1
			}
			if a < b {
				return -1
			}
			if a > b {
				return 1
			}
			return 0
		})
		for pos := 1; pos <= n; pos++ {
			idx := f.perm[pos-1]
			if vals[idx] == NoRank {
				continue
			}
			f.scores[idx] += 1.0 / (k + float64(pos))
		}
	}

	maxScore := float64(s) / (k + 1.0)
	scale := 65535.0 / maxScore
	var skipped uint64
	for i := 0; i < n; i++ {
		sc := f.scores[i]
		if sc <= 0 {
			dst[i] = NoRank
			skipped++
			continue
		}
		r := int(sc*scale + 0.5)
		if r < 1 {
			r = 1
		}
		if r > 65535 {
			r = 65535
		}
		dst[i] = Rank(r)
	}
	if m != nil {
		m.Candidates += uint64(n)
		m.Skipped += skipped
		m.Scored += uint64(n) - skipped
	}
	return nil
}

// Reset clears reusable scratch while retaining capacity.
func (f *RRF) Reset() {
	clear(f.scores)
	f.scores = f.scores[:0]
	f.perm = f.perm[:0]
}
