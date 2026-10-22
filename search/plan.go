package search

import (
	"cmp"
	"context"
	"errors"
	"runtime"
	"slices"

	unified "github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	schema "github.com/evanoberholster/imageIdx/schema"

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
	Cost     uint64
	// Namespace is zero for timestamp and ACL stages.
	Namespace        schema.Namespace
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
	cost     uint64

	namespaceIndex    *bitmapidx.Index
	namespaceKind     namespaceClauseKind
	namespaceQuery    bitmapidx.Query
	namespaceRange    bitmapidx.NumericRange
	namespaceBSIRange bitmapidx.ValueRange

	tsIndex *timeidx.Index
	tsStart uint64
	tsEnd   uint64
	tsExact timeidx.ExactTimestampFunc

	geoIndex  *geoidx.Index
	geoRanges []geoidx.Range

	aclQueries []bitmapidx.Query
}

// stageCost is a bounded, deliberately small cost model. It estimates the
// relative work of materializing/filtering a stage from its cardinality and
// operation family; it is not a general optimizer or a promise of exact
// selectivity. Costs saturate rather than overflow.
func stageCost(s *stage) uint64 {
	estimate := s.estimate
	var fixed, per uint64
	switch s.kind {
	case StageNamespace:
		switch s.namespaceKind {
		case namespaceValues, namespaceText:
			fixed, per = 1, 1
		case namespaceRange:
			fixed, per = 2, 2
		case namespaceBSIRange:
			fixed, per = 4, 3
		default:
			fixed, per = 2, 2
		}
	case StageTimestamp:
		fixed, per = 2, 2
		if s.tsExact == nil {
			fixed, per = 1, 1
		}
	case StageGeo:
		fixed, per = 8, 6
		fixed += uint64(len(s.geoRanges)) * 2
	case StageACL:
		fixed, per = 2, 2
	default:
		fixed, per = 4, 4
	}
	if estimate > (^uint64(0)-fixed)/max(per, 1) {
		return ^uint64(0)
	}
	return fixed + estimate*max(per, 1)
}

func (s *stage) metadata() Stage {
	meta := Stage{Kind: s.kind, Estimate: s.estimate, Cost: s.cost, CanProbe: s.canFilter(),
		OrderedSource:    s.kind != StageTimestamp,
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
		case namespaceBSIRange:
			meta.Predicate = "bsi-range"
		}
	case StageGeo:
		meta.Namespace = s.geoIndex.Namespace()
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
// only with an exact canonical lookup. Geo stages cannot verify a single
// candidate because geoidx keeps no ImageID -> key reverse index, so they are
// always collected as an ordered source and intersected.
func (s *stage) canFilter() bool {
	switch s.kind {
	case StageNamespace:
		return true
	case StageTimestamp:
		return s.tsExact != nil
	case StageGeo:
		return false
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
		if s.namespaceKind == namespaceBSIRange {
			match, err := s.namespaceIndex.BSIRangeQuery(s.namespaceBSIRange)
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
		return s.geoIndex.SearchRanges(dst, s.geoRanges), nil
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
		if s.namespaceKind == namespaceBSIRange {
			return s.namespaceIndex.BSIFilterRange(dst, candidates, s.namespaceBSIRange)
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
		// Geo is never a probe stage (canFilter is false); handled by collect.
		return dst, ErrInvalidClause
	case StageACL:
		return s.filterACL(dst, candidates, ws)
	default:
		return dst, ErrInvalidClause
	}
}

// Plan is a reusable, cardinality-ordered cross-index query. Plans compiled by
// an Engine capture pinned engine snapshots at the start of each execution, so
// all stages observe one publication generation without blocking writers.
// Directly compiled plans hold live index pointers and must keep their indexes
// open for the plan's lifetime.
type Plan struct {
	stages []stage
	// guard is set by Engine.Compile. It pins the unified store's family views
	// for the full execution of a query so engine reads cannot mix generations.
	guard *unified.Store
	// empty marks a valid plan that matches nothing (e.g. intersected
	// timestamp ranges with start >= end). Execution returns dst unchanged.
	empty bool
}

// beginQuery captures one immutable set of engine snapshots at a quiescent
// publication sequence and returns a plan that reads only those snapshots. The
// returned release function must be called after execution. The capture is
// optimistic: if a writer starts while the snapshots are being taken, all
// snapshots are discarded and the capture is retried. Once captured, readers
// execute without a writer lock and writers publish independently. Engine
// reclamation may still wait for a pinned old mapping before unmapping it.
func (plan Plan) beginQuery(ctx context.Context) (Plan, func(), error) {
	if plan.guard == nil || plan.empty {
		return plan, func() {}, nil
	}
	for {
		seq, err := plan.guard.BeginQuerySnapshot(ctx)
		if err != nil {
			return Plan{}, nil, err
		}
		snapshot := plan
		snapshot.stages = slices.Clone(plan.stages)
		releases := make([]func(), 0, len(snapshot.stages))
		for n := range snapshot.stages {
			snapshot.stages[n].snapshot(&releases)
		}
		if plan.guard.QuerySnapshotValid(seq) {
			return snapshot, releaseAll(releases), nil
		}
		releaseAll(releases)()
		if err := ctx.Err(); err != nil {
			return Plan{}, nil, err
		}
		runtime.Gosched()
	}
}

func releaseAll(releases []func()) func() {
	return func() {
		for n := len(releases) - 1; n >= 0; n-- {
			releases[n]()
		}
	}
}

// snapshot replaces every engine handle in the stage with a pinned handle.
// The snapshot calls run only while the store's publication sequence is
// quiescent; the caller validates the sequence before using the result.
func (s *stage) snapshot(releases *[]func()) {
	if s.namespaceIndex != nil {
		s.namespaceIndex = s.namespaceIndex.Snapshot()
		index := s.namespaceIndex
		*releases = append(*releases, index.ReleaseSnapshot)
	}
	s.namespaceQuery = s.namespaceQuery.Snapshot()
	query := s.namespaceQuery
	*releases = append(*releases, query.ReleaseSnapshot)
	if s.tsIndex != nil {
		s.tsIndex = s.tsIndex.Snapshot()
		index := s.tsIndex
		*releases = append(*releases, index.ReleaseSnapshot)
	}
	if s.geoIndex != nil {
		s.geoIndex = s.geoIndex.Snapshot()
		index := s.geoIndex
		*releases = append(*releases, index.ReleaseSnapshot)
	}
	if len(s.aclQueries) != 0 {
		// Stage copies share the compiled ACL query slice. Clone it before
		// replacing entries so concurrent queries never mutate the reusable
		// plan or each other's snapshots.
		s.aclQueries = slices.Clone(s.aclQueries)
		for n := range s.aclQueries {
			s.aclQueries[n] = s.aclQueries[n].Snapshot()
			query := s.aclQueries[n]
			*releases = append(*releases, query.ReleaseSnapshot)
		}
	}
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
			case namespaceBSIRange:
				st.namespaceBSIRange = value.bsiBounds
				st.estimate, err = value.index.BSICountRange(value.bsiBounds)
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
				ranges, err := geoidx.RangesFromCellIDs(nil, value.cells)
				if err != nil {
					return Plan{}, errors.Join(ErrInvalidClause, err)
				}
				value.ranges = ranges
			}
			if value.index == nil || len(value.ranges) == 0 {
				return Plan{}, ErrInvalidClause
			}
			ordered = append(ordered, orderedStage{
				s: stage{
					kind:      StageGeo,
					estimate:  value.index.EstimateRanges(value.ranges),
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
		leftCost, rightCost := stageCost(&left.s), stageCost(&right.s)
		if leftCost != rightCost {
			return cmp.Compare(leftCost, rightCost)
		}
		return cmp.Compare(left.order, right.order)
	})
	stages := make([]stage, len(ordered))
	for i := range ordered {
		stages[i] = ordered[i].s
		stages[i].cost = stageCost(&stages[i])
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
			} else if st.namespaceKind == namespaceBSIRange {
				st.estimate, err = st.namespaceIndex.BSICountRange(st.namespaceBSIRange)
			} else {
				st.estimate = st.namespaceQuery.Estimate()
			}
		case StageGeo:
			st.estimate = st.geoIndex.EstimateRanges(st.geoRanges)
		case StageTimestamp:
			st.estimate = st.tsIndex.EstimateRange(st.tsStart, st.tsEnd)
		case StageACL:
			st.estimate = aclEstimate(st.aclQueries)
		}
		if err != nil {
			return Plan{}, err
		}
		st.cost = stageCost(st)
	}
	slices.SortStableFunc(stages, func(a, b stage) int {
		if a.cost != b.cost {
			return cmp.Compare(a.cost, b.cost)
		}
		return cmp.Compare(a.estimate, b.estimate)
	})
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	return Plan{stages: stages, empty: plan.empty, guard: plan.guard}, nil
}
