package search

import (
	"errors"
	"slices"
	"sort"

	"github.com/evanoberholster/imageIdx/bitmapidx"

	"github.com/evanoberholster/imageIdx/timeidx"
)

type StageKind uint8

const (
	StageField StageKind = iota + 1
	StageTimestamp
	StageGeo
)

func (kind StageKind) String() string {
	switch kind {
	case StageField:
		return "field"
	case StageTimestamp:
		return "timestamp"
	case StageGeo:
		return "geo"
	default:
		return "unknown"
	}
}

// Stage describes one compiled candidate stage.
type Stage struct {
	Kind     StageKind
	Estimate uint64
}

// stage is the unified compiled unit. Estimates are cached at Compile time
// so ordering never re-queries live indexes. Value semantics avoid interface
// boxing and pointer/value receiver mix.
type stage struct {
	kind     StageKind
	estimate uint64

	fieldIndex *bitmapidx.FieldIndex
	fieldKind  fieldClauseKind
	fieldQuery bitmapidx.Query
	fieldRange bitmapidx.NumericRange

	tsIndex *timeidx.Index
	tsStart uint64
	tsEnd   uint64
	tsExact timeidx.ExactTimestampFunc

	geoIndex  *bitmapidx.FieldIndex
	geoRanges []bitmapidx.GeoRange
}

func (s *stage) metadata() Stage {
	return Stage{Kind: s.kind, Estimate: s.estimate}
}

// canFilter reports whether filter can verify candidates without collecting.
// Field stages always filter via their typed field query. Timestamp stages filter
// only with an exact canonical lookup. Geo stages filter by verifying
// candidates against covered cells; filter itself falls back to
// collect+intersect when the covered population is small relative to the
// candidate set.
func (s *stage) canFilter() bool {
	switch s.kind {
	case StageField:
		return true
	case StageTimestamp:
		return s.tsExact != nil
	case StageGeo:
		return true
	default:
		return false
	}
}

func (s *stage) collect(dst []ImageID) []ImageID {
	switch s.kind {
	case StageField:
		if s.fieldKind == fieldRange {
			match, err := s.fieldIndex.Range(s.fieldRange)
			if err != nil {
				return dst
			}
			return match.AppendTo(dst)
		}
		match, err := s.fieldQuery.Match()
		if err != nil {
			return dst
		}
		return match.AppendTo(dst)
	case StageTimestamp:
		if s.tsExact != nil {
			return s.tsIndex.SearchExact(dst, s.tsStart, s.tsEnd, s.tsExact)
		}
		return s.tsIndex.Search(dst, s.tsStart, s.tsEnd)
	case StageGeo:
		match, err := s.geoIndex.GeoRanges(s.geoRanges)
		if err != nil {
			return dst
		}
		return match.AppendTo(dst)
	default:
		return dst
	}
}

func (s *stage) filter(dst, candidates, scratch []ImageID, ws *Workspace) []ImageID {
	switch s.kind {
	case StageField:
		if s.fieldKind == fieldRange {
			filtered, err := s.fieldIndex.FilterRange(dst, candidates, s.fieldRange)
			if err != nil {
				return dst
			}
			return filtered
		}
		filtered, err := s.fieldQuery.Filter(dst, candidates)
		if err != nil {
			return dst
		}
		return filtered
	case StageTimestamp:
		if s.tsExact == nil {
			return append(dst, candidates...)
		}
		for _, id := range candidates {
			value, ok := s.tsExact(id)
			if ok && value >= s.tsStart && value < s.tsEnd {
				dst = append(dst, id)
			}
		}
		return dst
	case StageGeo:
		return geoFilter(dst, candidates, scratch, ws, s)
	default:
		return append(dst, candidates...)
	}
}

// geoFilterProbeBound bounds the verify-vs-collect decision for geo stages.
// Verifying costs about one cursor probe per (cell, candidate) pair while
// collecting costs a materialize plus sort per covered ID. Dense level-16
// cell measurements put the crossover near probes <= 4*population.
const geoFilterProbeBound = 4

// geoFilter restricts candidates to the stage's ranges. Few candidates
// relative to the covered population verify in place against covered cells;
// otherwise it collects the ranges into scratch through the Search-owned
// MutableBitmap and intersects as before, so large geo stages keep the
// workspace-reuse profile. Either path returns a sorted subset of candidates.
func geoFilter(dst, candidates, scratch []ImageID, ws *Workspace, s *stage) []ImageID {
	if s.geoIndex == nil || len(s.geoRanges) == 0 || s.estimate == 0 {
		return dst
	}
	if cells := s.geoIndex.CountGeoCells(s.geoRanges); uint64(cells)*uint64(len(candidates)) <= geoFilterProbeBound*s.estimate {
		start := len(dst)
		filtered, err := s.geoIndex.FilterGeoRanges(dst, candidates, s.geoRanges)
		if err != nil {
			return dst
		}
		dst = filtered
		slices.Sort(dst[start:])
		return dst
	}
	scratch = collectStage(scratch[:0], ws, s)
	start := len(dst)
	dst = append(dst, candidates...)
	return intersectSortedSuffix(dst, start, scratch)
}

// Plan is a reusable, cardinality-ordered cross-index query.
type Plan struct {
	stages []stage
	// empty marks a valid plan that matches nothing (e.g. intersected
	// timestamp ranges with start >= end). Execution returns dst unchanged.
	empty bool
}

// IsEmpty reports whether the plan is valid but matches nothing.
func (plan Plan) IsEmpty() bool {
	return plan.empty || len(plan.stages) == 0
}

// Len returns the number of compiled stages.
func (plan Plan) Len() int {
	return len(plan.stages)
}

// Estimate returns the smallest stage estimate, or 0 for empty plans.
// Useful for pre-sizing destination and workspace buffers.
func (plan Plan) Estimate() uint64 {
	if plan.empty || len(plan.stages) == 0 {
		return 0
	}
	min := plan.stages[0].estimate
	for _, s := range plan.stages[1:] {
		if s.estimate < min {
			min = s.estimate
		}
	}
	return min
}

// timestampRange is a coalesced lossy-only range for one index.
type timestampRange struct {
	start uint64
	end   uint64
	order int
}

// Compile validates field clauses, coalesces
// lossy-only timestamp ranges on the same index by intersection, and orders
// the resulting stages by their current cardinality estimates. Ties keep
// clause order is retained for equal estimates.
func Compile(clauses ...Clause) (Plan, error) {
	if len(clauses) == 0 {
		return Plan{}, ErrEmptyQuery
	}

	lossyRanges := make(map[*timeidx.Index]*timestampRange)
	type orderedStage struct {
		s     stage
		order int
	}
	var ordered []orderedStage

	flushLossy := func() {
		for index, r := range lossyRanges {
			ordered = append(ordered, orderedStage{
				s: stage{
					kind:     StageTimestamp,
					estimate: index.EstimateRange(r.start, r.end),
					tsIndex:  index,
					tsStart:  r.start,
					tsEnd:    r.end,
				},
				order: r.order,
			})
		}
		for k := range lossyRanges {
			delete(lossyRanges, k)
		}
	}

	for clauseIndex, clause := range clauses {
		switch value := clause.(type) {
		case fieldClause:
			if value.index == nil {
				return Plan{}, ErrInvalidClause
			}
			st := stage{kind: StageField, fieldIndex: value.index, fieldKind: value.kind}
			var err error
			switch value.kind {
			case fieldValues:
				if len(value.values) == 0 {
					return Plan{}, ErrInvalidClause
				}
				st.fieldQuery, err = value.index.Compile(value.values...)
				if err == nil {
					st.estimate, err = st.fieldQuery.Count()
				}
			case fieldText:
				st.fieldQuery, err = value.index.CompileText(value.text)
				if err == nil {
					st.estimate, err = st.fieldQuery.Count()
				}
			case fieldRange:
				st.fieldRange = value.bounds
				st.estimate, err = value.index.EstimateRange(value.bounds)
			default:
				return Plan{}, ErrInvalidClause
			}
			if err != nil {
				return Plan{}, errors.Join(ErrInvalidClause, err)
			}
			ordered = append(ordered, orderedStage{s: st, order: clauseIndex})
		case timestampClause:
			if value.index == nil {
				return Plan{}, ErrInvalidClause
			}
			if value.start >= value.end {
				// Valid but empty: short-circuit without touching indexes.
				return Plan{empty: true}, nil
			}
			if value.exact == nil {
				if existing, ok := lossyRanges[value.index]; ok {
					if value.start > existing.start {
						existing.start = value.start
					}
					if value.end < existing.end {
						existing.end = value.end
					}
					if clauseIndex < existing.order {
						existing.order = clauseIndex
					}
					if existing.start >= existing.end {
						return Plan{empty: true}, nil
					}
				} else {
					lossyRanges[value.index] = &timestampRange{start: value.start, end: value.end, order: clauseIndex}
				}
				continue
			}
			ordered = append(ordered, orderedStage{
				s: stage{
					kind:     StageTimestamp,
					estimate: value.index.EstimateRange(value.start, value.end),
					tsIndex:  value.index,
					tsStart:  value.start,
					tsEnd:    value.end,
					tsExact:  value.exact,
				},
				order: clauseIndex,
			})
		case geoClause:
			if value.index == nil || len(value.ranges) == 0 {
				return Plan{}, ErrInvalidClause
			}
			estimate, err := value.index.EstimateGeoRanges(value.ranges)
			if err != nil {
				return Plan{}, ErrInvalidClause
			}
			ordered = append(ordered, orderedStage{
				s: stage{
					kind:      StageGeo,
					estimate:  estimate,
					geoIndex:  value.index,
					geoRanges: value.ranges,
				},
				order: clauseIndex,
			})
		default:
			return Plan{}, ErrInvalidClause
		}
	}
	flushLossy()

	// Fast path: any zero estimate means the intersection is empty, but
	// estimates are upper bounds and may be stale, so do not mark empty here.
	// Execution still short-circuits once candidates hit zero.
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].s.estimate != ordered[right].s.estimate {
			return ordered[left].s.estimate < ordered[right].s.estimate
		}
		return ordered[left].order < ordered[right].order
	})
	stages := make([]stage, len(ordered))
	for i := range ordered {
		stages[i] = ordered[i].s
	}
	return Plan{stages: stages}, nil
}

// MustCompile is like Compile but panics on error.
func MustCompile(clauses ...Clause) Plan {
	plan, err := Compile(clauses...)
	if err != nil {
		panic(err)
	}
	return plan
}

// Stages returns a copy of plan metadata in execution order.
func (plan Plan) Stages() []Stage {
	result := make([]Stage, len(plan.stages))
	for index := range plan.stages {
		result[index] = plan.stages[index].metadata()
	}
	return result
}

// Explain returns human-readable stage metadata in execution order.
func (plan Plan) Explain() []Stage {
	return plan.Stages()
}
