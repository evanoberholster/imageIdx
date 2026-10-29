package metrics

import "time"

// SearchOperation identifies the execution measured by a report.
type SearchOperation uint8

const (
	OperationSearch SearchOperation = iota + 1
	OperationFilterIDs
	OperationRankTime
	OperationRankGeo
	OperationRankVector
)

// StageMode describes how a stage ran. A stage not reached because an
// earlier stage emptied the candidates has StageModeSkipped.
type StageMode uint8

const (
	StageModeSkipped StageMode = iota
	StageModeSource
	StageModeFilter
	StageModeCollectIntersect
	StageModeBufferedFilter
)

// RankMetrics describes rank-phase work. ScoreDuration includes canonical
// lookup, signal encoding, fusion scoring, and top-k heap maintenance,
// measured per batch or per call; SortDuration covers final ordering and
// Top-K selection sorts. Neither includes plan filter stages.
type RankMetrics struct {
	K             int
	Candidates    uint64
	Scored        uint64
	Skipped       uint64
	ScoreDuration time.Duration
	SortDuration  time.Duration
}

// Timer measures one phase into a shared duration accumulator without
// allocating. It keeps the ScoreDuration/SortDuration accumulation idiom in
// one place so search and ranking time phases identically:
//
//	t := metrics.Start()
//	... measured work ...
//	t.Add(&m.ScoreDuration)
type Timer struct {
	start time.Time
}

// Start returns a Timer anchored at now.
func Start() Timer {
	return Timer{start: time.Now()}
}

// Add accumulates the elapsed time since Start into dst.
func (t Timer) Add(dst *time.Duration) {
	*dst += time.Since(t.start)
}
