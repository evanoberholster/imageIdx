package search

import (
	"context"
	"time"
)

// SearchOperation identifies the execution measured by SearchMetrics.
type SearchOperation uint8

const (
	OperationSearch SearchOperation = iota + 1
	OperationFilterIDs
	OperationRankTime
	OperationRankGeo
)

// StageMode describes how a stage ran. A stage not reached because an earlier
// stage emptied the candidates has StageModeSkipped.
type StageMode uint8

const (
	StageModeSkipped StageMode = iota
	StageModeSource
	StageModeFilter
	StageModeCollectIntersect
	StageModeBufferedFilter
)

// StageMetrics describes one plan stage in the order returned by Plan.Stages.
// ExecutionOrder is -1 if the stage was not reached. For a source, Collected
// is the complete source population and Passed is the number visited; for a
// filter, Input and Passed count candidate entries (including duplicates).
// Duration includes index calls and any exact timestamp lookup for the stage.
type StageMetrics struct {
	Stage          Stage
	PlanIndex      int
	ExecutionOrder int
	Mode           StageMode
	Calls          uint64
	Input          uint64
	Passed         uint64
	Collected      uint64
	Duration       time.Duration
}

// Rejected is the number of input candidate entries rejected by this stage.
func (s StageMetrics) Rejected() uint64 {
	if s.Passed >= s.Input {
		return 0
	}
	return s.Input - s.Passed
}

// RankMetrics describes score work after plan filtering. ScoreDuration includes
// canonical lookup, scoring, and top-k heap maintenance, measured per batch;
// SortDuration covers the final result sort. Neither includes filter stages.
type RankMetrics struct {
	K             int
	Candidates    uint64
	Scored        uint64
	Skipped       uint64
	ScoreDuration time.Duration
	SortDuration  time.Duration
}

// SearchMetrics is one caller-owned execution report. Reuse it sequentially;
// never share one instance between concurrent searches. Counts describe work
// observed during this run, not a transaction-wide snapshot. Matched counts
// may be partial after cancellation or failure even though returned dst is
// truncated to its original length. TotalDuration is wall time.
type SearchMetrics struct {
	Operation      SearchOperation
	TotalDuration  time.Duration
	Matched        uint64
	Returned       uint64
	Complete       bool
	Stages         []StageMetrics
	Rank           RankMetrics
	executionCount int
}

// NewSearchMetrics preallocates stage records for plan. A zero value is also
// valid and grows its stage slice on the first measured run.
func NewSearchMetrics(plan Plan) *SearchMetrics {
	return &SearchMetrics{Stages: make([]StageMetrics, 0, plan.Len())}
}

func (m *SearchMetrics) reset(plan Plan, operation SearchOperation) {
	stages := m.Stages[:0]
	if cap(stages) < len(plan.stages) {
		stages = make([]StageMetrics, len(plan.stages))
	} else {
		stages = stages[:len(plan.stages)]
	}
	*m = SearchMetrics{Operation: operation, Stages: stages}
	for n := range plan.stages {
		m.Stages[n] = StageMetrics{
			Stage:          plan.stages[n].metadata(),
			PlanIndex:      n,
			ExecutionOrder: -1,
		}
	}
}

func (m *SearchMetrics) reach(index int, mode StageMode) *StageMetrics {
	stage := &m.Stages[index]
	if stage.ExecutionOrder < 0 {
		stage.ExecutionOrder = m.executionCount
		m.executionCount++
	}
	stage.Mode = mode
	stage.Calls++
	return stage
}

// Search runs the plan without instrumentation. It uses the same fast bulk
// execution as AppendIDs and appends sorted, unique IDs to dst.
func (plan Plan) Search(ctx context.Context, dst []ImageID, ws *Workspace) ([]ImageID, error) {
	return plan.AppendIDs(ctx, dst, ws)
}

// SearchWithMetrics runs the same bulk search while filling metrics. Passing
// nil metrics is an error; use Search for the uninstrumented fast path.
func (plan Plan) SearchWithMetrics(ctx context.Context, dst []ImageID, ws *Workspace, metrics *SearchMetrics) ([]ImageID, error) {
	if metrics == nil {
		return dst, ErrInvalidOptions
	}
	start := time.Now()
	metrics.reset(plan, OperationSearch)
	defer func() { metrics.TotalDuration = time.Since(start) }()
	initial := len(dst)
	if err := ctx.Err(); err != nil {
		return dst, err
	}
	out, err := plan.searchInto(ctx, dst, ws, metrics)
	metrics.Matched = uint64(len(out) - initial)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return out[:initial], err
	}
	metrics.Returned = metrics.Matched
	metrics.Complete = true
	return out, nil
}
