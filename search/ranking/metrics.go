package ranking

import (
	"errors"

	searchmetrics "github.com/evanoberholster/imageIdx/search/metrics"
)

// ErrInvalidMetrics reports a nil metrics report on a measured entry point.
// Use the uninstrumented fast path (Fuse, Sort, TopK, RankTimeSignal,
// EncodeDistances, QuantizeCosines) when no report is needed.
var ErrInvalidMetrics = errors.New("ranking: invalid metrics")

// RankTimeSignalWithMetrics is RankTimeSignal with scoring counts and timing
// added to m: Candidates counts ids, Scored counts lookups that hit,
// Skipped counts misses, ScoreDuration covers the loop.
func RankTimeSignalWithMetrics(dst []Rank, ids []ImageID, target uint64, lookup TimestampFunc, m *searchmetrics.RankMetrics) error {
	if m == nil {
		return ErrInvalidMetrics
	}
	t := searchmetrics.Start()
	rankTimeSignal(dst, ids, target, lookup, m)
	t.Add(&m.ScoreDuration)
	return nil
}

// EncodeDistancesWithMetrics is EncodeDistances with counts and timing added
// to m. Every distance encodes, so Scored always grows by len(src).
func EncodeDistancesWithMetrics(dst []Rank, src []uint32, m *searchmetrics.RankMetrics) error {
	if m == nil {
		return ErrInvalidMetrics
	}
	t := searchmetrics.Start()
	encodeDistances(dst, src, m)
	t.Add(&m.ScoreDuration)
	return nil
}

// QuantizeCosinesWithMetrics is QuantizeCosines with counts and timing added
// to m: NaN inputs count as Skipped and encode to NoRank.
func QuantizeCosinesWithMetrics(dst []Rank, src []float32, m *searchmetrics.RankMetrics) error {
	if m == nil {
		return ErrInvalidMetrics
	}
	t := searchmetrics.Start()
	quantizeCosines(dst, src, m)
	t.Add(&m.ScoreDuration)
	return nil
}

// FuseWithMetrics is RRF.Fuse with fusion counts and timing added to m:
// Candidates counts fused IDs, Scored counts fused outputs other than
// NoRank, Skipped counts NoRank outputs, ScoreDuration covers lane ordering
// plus score accumulation and quantization. The RRF damping constant is
// configuration and never overwrites m.K.
func (f *RRF) FuseWithMetrics(dst []Rank, src *Signals, m *searchmetrics.RankMetrics) error {
	if m == nil {
		return ErrInvalidMetrics
	}
	t := searchmetrics.Start()
	err := f.fuse(dst, src, m)
	t.Add(&m.ScoreDuration)
	return err
}

// SortWithMetrics is Sort with ordering time added to m.SortDuration. It
// reports ErrLengthMismatch on mismatched inputs where Sort no-ops.
func SortWithMetrics(ids []ImageID, ranks []Rank, m *searchmetrics.RankMetrics) error {
	if m == nil {
		return ErrInvalidMetrics
	}
	if len(ids) != len(ranks) {
		return ErrLengthMismatch
	}
	t := searchmetrics.Start()
	Sort(ids, ranks)
	t.Add(&m.SortDuration)
	return nil
}

// TopKWithMetrics is TopK with selection counts and timing: m.K records k,
// Candidates counts inputs, Scored counts returned heads, Skipped counts the
// remainder, SortDuration covers heap selection plus the final sort.
func TopKWithMetrics(dstIDs []ImageID, dstRanks []Rank, srcIDs []ImageID, srcRanks []Rank, k int, m *searchmetrics.RankMetrics) ([]ImageID, []Rank, error) {
	if m == nil {
		return dstIDs, dstRanks, ErrInvalidMetrics
	}
	if len(srcIDs) != len(srcRanks) {
		return dstIDs, dstRanks, ErrLengthMismatch
	}
	m.K = k
	t := searchmetrics.Start()
	outIDs, outRanks := TopK(dstIDs, dstRanks, srcIDs, srcRanks, k)
	t.Add(&m.SortDuration)
	if n := len(srcIDs); k != 0 && n != 0 {
		m.Candidates += uint64(n)
		m.Scored += uint64(len(outIDs))
		m.Skipped += uint64(n) - uint64(len(outIDs))
	}
	return outIDs, outRanks, nil
}
