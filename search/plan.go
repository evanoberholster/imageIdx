package search

import (
	"cmp"
	"context"
	"errors"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"

	"github.com/evanoberholster/imageIdx/index/timeidx"
)

type StageKind uint8

const (
	StageNamespace StageKind = iota + 1
	StageTimestamp
	StageGeo
	StageACL
)

func (kind StageKind) String() string {
	switch kind {
	case StageNamespace:
		return "namespace"
	case StageTimestamp:
		return "timestamp"
	case StageGeo:
		return "geo"
	case StageACL:
		return "acl"
	default:
		return "unknown"
	}
}

// Stage describes one compiled candidate stage.
type Stage struct {
	Kind     StageKind
	Estimate uint64
	// Namespace is zero for timestamp and ACL stages. Predicate describes semantics.
	Namespace        common.Namespace
	Predicate        string
	CanProbe         bool
	OrderedSource    bool
	BufferedFallback bool
}

// stage is the unified compiled unit. Estimates are cached at Compile time
// so ordering never re-queries live indexes. Value semantics avoid interface
// boxing and pointer/value receiver mix.
type stage struct {
	kind     StageKind
	estimate uint64

	namespaceIndex *bitmapidx.Index
	namespaceKind  namespaceClauseKind
	namespaceQuery bitmapidx.Query
	namespaceRange bitmapidx.NumericRange

	tsIndex *timeidx.Index
	tsStart uint64
	tsEnd   uint64
	tsExact timeidx.ExactTimestampFunc

	geoIndex  *bitmapidx.Index
	geoRanges []bitmapidx.GeoRange

	aclQueries []bitmapidx.Query
}

func (s *stage) metadata() Stage {
	meta := Stage{Kind: s.kind, Estimate: s.estimate, CanProbe: s.canFilter(), OrderedSource: s.kind != StageTimestamp,
		BufferedFallback: s.kind == StageTimestamp && s.tsExact == nil}
	switch s.kind {
	case StageNamespace:
		meta.Namespace = s.namespaceIndex.Spec().Namespace
		switch s.namespaceKind {
		case namespaceValues:
			meta.Predicate = "all-values"
		case namespaceText:
			meta.Predicate = "all-tokens"
		case namespaceRange:
			meta.Predicate = "numeric-range"
		}
	case StageGeo:
		meta.Namespace = s.geoIndex.Spec().Namespace
		meta.Predicate = "cell-union"
	case StageTimestamp:
		meta.Predicate = "timestamp-range"
	case StageACL:
		meta.Predicate = "any-principal"
	}
	return meta
}

// canFilter reports whether filter can verify candidates without collecting.
// Namespace stages always filter via their typed namespace query. Timestamp stages filter
// only with an exact canonical lookup. Geo stages filter by verifying
// candidates against covered cells; filter itself falls back to
// collect+intersect when the covered population is small relative to the
// candidate set.
func (s *stage) canFilter() bool {
	switch s.kind {
	case StageNamespace:
		return true
	case StageTimestamp:
		return s.tsExact != nil
	case StageGeo:
		return true
	case StageACL:
		return true
	default:
		return false
	}
}

func (s *stage) collect(dst []ImageID) ([]ImageID, error) {
	switch s.kind {
	case StageNamespace:
		if s.namespaceKind == namespaceRange {
			match, err := s.namespaceIndex.Range(s.namespaceRange)
			if err != nil {
				return dst, err
			}
			return match.AppendTo(dst), nil
		}
		match, err := s.namespaceQuery.Match()
		if err != nil {
			return dst, err
		}
		return match.AppendTo(dst), nil
	case StageTimestamp:
		if s.tsExact != nil {
			return s.tsIndex.SearchExact(dst, s.tsStart, s.tsEnd, s.tsExact), nil
		}
		return s.tsIndex.Search(dst, s.tsStart, s.tsEnd), nil
	case StageGeo:
		match, err := s.geoIndex.GeoRanges(s.geoRanges)
		if err != nil {
			return dst, err
		}
		return match.AppendTo(dst), nil
	case StageACL:
		match, err := s.aclBitmap()
		if err != nil {
			return dst, err
		}
		return match.AppendTo(dst), nil
	default:
		return dst, ErrInvalidClause
	}
}

func (s *stage) filter(dst, candidates, scratch []ImageID, ws *Workspace) ([]ImageID, error) {
	switch s.kind {
	case StageNamespace:
		if s.namespaceKind == namespaceRange {
			filtered, err := s.namespaceIndex.FilterRange(dst, candidates, s.namespaceRange)
			return filtered, err
		}
		filtered, err := s.namespaceQuery.Filter(dst, candidates)
		return filtered, err
	case StageTimestamp:
		if s.tsExact == nil {
			return append(dst, candidates...), nil
		}
		for _, id := range candidates {
			value, ok := s.tsExact(id)
			if ok && value >= s.tsStart && value < s.tsEnd {
				dst = append(dst, id)
			}
		}
		return dst, nil
	case StageGeo:
		return geoFilter(dst, candidates, scratch, ws, s)
	case StageACL:
		return s.filterACL(dst, candidates, ws)
	default:
		return dst, ErrInvalidClause
	}
}

// geoFilterProbeBound bounds the verify-vs-collect decision for geo stages.
// Verifying costs about one cursor probe per (cell, candidate) pair while
// collecting costs a materialize plus sort per covered ID. Dense level-16
// cell measurements put the crossover near probes <= 4*population.
const geoFilterProbeBound = 4

// geoFilter restricts candidates to the stage's ranges. Candidates must be
// ascending; the result preserves that order. Few candidates relative to the
// covered population verify in place against covered cells; otherwise it
// collects the ranges into scratch through the Search-owned MutableBitmap and
// intersects as before, so large geo stages keep the workspace-reuse profile.
func geoFilter(dst, candidates, scratch []ImageID, ws *Workspace, s *stage) ([]ImageID, error) {
	if s.geoIndex == nil || len(s.geoRanges) == 0 {
		return dst, nil
	}
	if cells := s.geoIndex.CountGeoCells(s.geoRanges); uint64(cells)*uint64(len(candidates)) <= geoFilterProbeBound*s.estimate {
		filtered, err := s.geoIndex.FilterGeoRanges(dst, candidates, s.geoRanges)
		return filtered, err
	}
	var err error
	scratch, err = collectStage(scratch[:0], ws, s)
	if err != nil {
		return dst, err
	}
	if ws != nil {
		ws.scratch = scratch
	}
	start := len(dst)
	dst = append(dst, candidates...)
	return intersectSortedSuffix(dst, start, scratch), nil
}

// Plan is a reusable, cardinality-ordered cross-index query. It holds raw
// index pointers: querying after an index closes matches nothing from
// that stage (closed indexes report empty), so keep the indexes open for
// the plan's lifetime.
type Plan struct {
	stages []stage
	// empty marks a valid plan that matches nothing (e.g. intersected
	// timestamp ranges with start >= end). Execution returns dst unchanged.
	empty bool
}

// IsEmpty reports structural emptiness, not whether live indexes currently
// have matches. Use Exists for that distinction.
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

// Compile validates namespace clauses, coalesces
// lossy-only timestamp ranges on the same index by intersection, and orders
// the resulting stages by their current cardinality estimates. Ties retain
// clause order.
func Compile(clauses ...Clause) (Plan, error) {
	return compile(nil, clauses...)
}

// CompileWithStore binds WithACL clauses to the store's UserACL and GroupACL
// namespaces. Plans keep compiled namespace queries, so the store must stay open while
// they execute. Other clauses compile identically to Compile.
func CompileWithStore(store *bitmapidx.Store, clauses ...Clause) (Plan, error) {
	return compile(store, clauses...)
}

func compile(store *bitmapidx.Store, clauses ...Clause) (Plan, error) {
	if len(clauses) == 0 {
		return Plan{}, ErrEmptyQuery
	}

	lossyRanges := make(map[*timeidx.Index]*timestampRange)
	type orderedStage struct {
		s     stage
		order int
	}
	var ordered []orderedStage
	empty := false

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
		case aclClause:
			st, err := compileACL(store, value)
			if err != nil {
				return Plan{}, errors.Join(ErrInvalidClause, err)
			}
			ordered = append(ordered, orderedStage{s: st, order: clauseIndex})
		case namespaceClause:
			if value.index == nil {
				return Plan{}, ErrInvalidClause
			}
			st := stage{kind: StageNamespace, namespaceIndex: value.index, namespaceKind: value.kind}
			var err error
			switch value.kind {
			case namespaceValues:
				if len(value.values) == 0 {
					return Plan{}, ErrInvalidClause
				}
				st.namespaceQuery, err = value.index.Compile(value.values...)
				if err == nil {
					st.estimate = st.namespaceQuery.Estimate()
				}
			case namespaceText:
				st.namespaceQuery, err = value.index.CompileText(value.text)
				if err == nil {
					st.estimate = st.namespaceQuery.Estimate()
				}
			case namespaceRange:
				st.namespaceRange = value.bounds
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
				// Still validate subsequent clauses, independent of clause order.
				empty = true
				continue
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
						empty = true
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
			if len(value.cells) != 0 {
				ranges, err := bitmapidx.RangesFromCellIDs(nil, value.cells)
				if err != nil {
					return Plan{}, errors.Join(ErrInvalidClause, err)
				}
				value.ranges = ranges
			}
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
	// flushLossy drains a map, but order stays deterministic: every range
	// carries its minimum clause index, and the stable sort below breaks
	// estimate ties on it, so map iteration order never leaks through.
	flushLossy()

	// Estimates may be stale. A zero estimate affects order, not correctness;
	// execution short-circuits only when live candidates are empty.
	slices.SortStableFunc(ordered, func(left, right orderedStage) int {
		if left.s.estimate != right.s.estimate {
			return cmp.Compare(left.s.estimate, right.s.estimate)
		}
		return cmp.Compare(left.order, right.order)
	})
	stages := make([]stage, len(ordered))
	for i := range ordered {
		stages[i] = ordered[i].s
	}
	return Plan{stages: stages, empty: empty}, nil
}

// Stages returns a copy of plan metadata in execution order. StageKind
// renders via its String method, so no separate explain view exists.
func (plan Plan) Stages() []Stage {
	result := make([]Stage, len(plan.stages))
	for index := range plan.stages {
		result[index] = plan.stages[index].metadata()
	}
	return result
}

// Refresh returns a new immutable plan with inexpensive planning estimates
// and updated stage order. It reuses canonicalized predicates; no analyzer or
// hashing runs again. The old plan stays valid for concurrent executions.
// Refresh explicitly after workload changes, not on every query.
func (plan Plan) Refresh(ctx context.Context) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	stages := slices.Clone(plan.stages)
	for n := range stages {
		if err := ctx.Err(); err != nil {
			return Plan{}, err
		}
		st := &stages[n]
		var err error
		switch st.kind {
		case StageNamespace:
			if st.namespaceKind == namespaceRange {
				st.estimate, err = st.namespaceIndex.EstimateRange(st.namespaceRange)
			} else {
				st.estimate = st.namespaceQuery.Estimate()
			}
		case StageGeo:
			st.estimate, err = st.geoIndex.EstimateGeoRanges(st.geoRanges)
		case StageTimestamp:
			st.estimate = st.tsIndex.EstimateRange(st.tsStart, st.tsEnd)
		case StageACL:
			st.estimate = aclEstimate(st.aclQueries)
		}
		if err != nil {
			return Plan{}, err
		}
	}
	slices.SortStableFunc(stages, func(a, b stage) int { return cmp.Compare(a.estimate, b.estimate) })
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	return Plan{stages: stages, empty: plan.empty}, nil
}
