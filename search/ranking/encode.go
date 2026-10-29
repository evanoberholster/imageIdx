package ranking

import (
	"math"
	"unsafe"

	searchmetrics "github.com/evanoberholster/imageIdx/search/metrics"
	"github.com/evanoberholster/imageIdx/search/ranking/internal/rankkernel"
)

// EncodeDistance maps a native uint32 distance (seconds for time,
// centimeters for geo) to a Rank. Higher is better: 0 distance -> MaxRank,
// larger distances -> smaller ranks down to MinRank. It never returns NoRank;
// missing values must be written as NoRank explicitly by the caller.
func EncodeDistance(d uint32) Rank { return Rank(rankkernel.EncodeDistance(d)) }

// EncodeTimeDistance maps seconds to Rank.
func EncodeTimeDistance(d TimeDistance) Rank { return EncodeDistance(uint32(d)) }

// EncodeGeoDistance maps centimeters to Rank.
func EncodeGeoDistance(d GeoDistance) Rank { return EncodeDistance(uint32(d)) }

// EncodeTimeGap maps |stamp-target| to Rank, saturating gaps above MaxUint32
// to the worst ordinary rank. Exact equality maps to MaxRank.
func EncodeTimeGap(target, stamp uint64) Rank {
	var d uint64
	if stamp >= target {
		d = stamp - target
	} else {
		d = target - stamp
	}
	if d > 0xFFFFFFFF {
		return MinRank
	}
	return EncodeDistance(uint32(d))
}

// QuantizeCosine maps cosine similarity to Rank: +1 -> MaxRank, -1 ->
// MinRank, NaN -> NoRank. Out-of-range values clamp. Higher is better.
func QuantizeCosine(sim float32) Rank { return Rank(rankkernel.QuantizeCosine(sim)) }

// QuantizeCosineDistance maps vectoridx's cosine distance (1-cos, smaller is
// better) to Rank. NaN maps to NoRank.
func QuantizeCosineDistance(distance float32) Rank {
	return QuantizeCosine(1 - distance)
}

// EncodeDistances writes one rank per native distance. Caller-owned dst must
// hold at least len(src) slots. Zero allocations.
func EncodeDistances(dst []Rank, src []uint32) {
	encodeDistances(dst, src, nil)
}

func encodeDistances(dst []Rank, src []uint32, m *searchmetrics.RankMetrics) {
	if len(src) == 0 {
		return
	}
	rankkernel.EncodeDistances(*(*[]uint16)(unsafe.Pointer(&dst)), src)
	if m != nil {
		m.Candidates += uint64(len(src))
		m.Scored += uint64(len(src))
	}
}

// QuantizeCosines writes one rank per cosine similarity. NaN -> NoRank.
func QuantizeCosines(dst []Rank, src []float32) {
	quantizeCosines(dst, src, nil)
}

func quantizeCosines(dst []Rank, src []float32, m *searchmetrics.RankMetrics) {
	if len(src) == 0 {
		return
	}
	rankkernel.QuantizeCosines(*(*[]uint16)(unsafe.Pointer(&dst)), src)
	if m != nil {
		var skipped uint64
		for _, v := range src {
			if math.IsNaN(float64(v)) {
				skipped++
			}
		}
		m.Candidates += uint64(len(src))
		m.Skipped += skipped
		m.Scored += uint64(len(src)) - skipped
	}
}

// TimestampFunc supplies canonical timestamps for lookup-based time ranking.
// It mirrors timeidx.ExactTimestampFunc without importing the index package,
// keeping ranking free of index dependencies. ok=false means missing.
type TimestampFunc func(id ImageID) (uint64, bool)

// RankTimeSignal writes one time rank per id: exact target -> MaxRank,
// farther -> smaller, missing -> NoRank. dst must hold len(ids) slots.
func RankTimeSignal(dst []Rank, ids []ImageID, target uint64, lookup TimestampFunc) {
	rankTimeSignal(dst, ids, target, lookup, nil)
}

func rankTimeSignal(dst []Rank, ids []ImageID, target uint64, lookup TimestampFunc, m *searchmetrics.RankMetrics) {
	if lookup == nil {
		for i := range ids {
			dst[i] = NoRank
		}
		if m != nil {
			m.Candidates += uint64(len(ids))
			m.Skipped += uint64(len(ids))
		}
		return
	}
	var skipped uint64
	for i, id := range ids {
		stamp, ok := lookup(id)
		if !ok {
			dst[i] = NoRank
			skipped++
			continue
		}
		dst[i] = EncodeTimeGap(target, stamp)
	}
	if m != nil {
		m.Candidates += uint64(len(ids))
		m.Skipped += skipped
		m.Scored += uint64(len(ids)) - skipped
	}
}
