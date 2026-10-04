package search

import (
	"context"
	"slices"
	"time"
)

const executionBatch = 256

// FilterIDs appends matching candidates, preserving their order and duplicates.
// dst=candidates[:0] is supported; other overlapping layouts are not supported.
// On error the returned slice has its original length. Its appended backing
// storage (and an in-place candidate slice) may have been overwritten.
// Lossy timestamp stages without exact lookups require a buffered fallback.
func (plan Plan) FilterIDs(ctx context.Context, dst, candidates []ImageID, ws *Workspace) ([]ImageID, error) {
	return plan.filterIDs(ctx, dst, candidates, ws, nil)
}

// FilterIDsWithMetrics is FilterIDs with per-stage candidate and timing data.
// metrics is reset before execution and must not be used concurrently.
func (plan Plan) FilterIDsWithMetrics(ctx context.Context, dst, candidates []ImageID, ws *Workspace, metrics *SearchMetrics) ([]ImageID, error) {
	if metrics == nil {
		return dst, ErrInvalidOptions
	}
	start := time.Now()
	metrics.reset(plan, OperationFilterIDs)
	defer func() { metrics.TotalDuration = time.Since(start) }()
	return plan.filterIDs(ctx, dst, candidates, ws, metrics)
}

func (plan Plan) filterIDs(ctx context.Context, dst, candidates []ImageID, ws *Workspace, metrics *SearchMetrics) ([]ImageID, error) {
	start := len(dst)
	if err := ctx.Err(); err != nil {
		return dst, err
	}
	if plan.IsEmpty() || len(candidates) == 0 {
		if metrics != nil {
			metrics.Complete = true
		}
		return dst, nil
	}
	if ws == nil {
		ws = &Workspace{}
	}
	if err := plan.prepareProbes(ctx, ws, 0, metrics); err != nil {
		return dst, err
	}
	for offset := 0; offset < len(candidates); offset += executionBatch {
		batch := candidates[offset:min(offset+executionBatch, len(candidates))]
		// A private bounded input buffer avoids mutating caller candidates.
		ws.batch = append(ws.batch[:0], batch...)
		matched, err := plan.probe(ctx, ws.batch, ws, 0, metrics)
		if err != nil {
			if metrics != nil {
				metrics.Matched = uint64(len(dst) - start)
			}
			return dst[:start], err
		}
		dst = append(dst, matched...)
	}
	if err := ctx.Err(); err != nil {
		if metrics != nil {
			metrics.Matched = uint64(len(dst) - start)
		}
		return dst[:start], err
	}
	if metrics != nil {
		metrics.Matched = uint64(len(dst) - start)
		metrics.Returned = metrics.Matched
		metrics.Complete = true
	}
	return dst, nil
}

func (plan Plan) prepareProbes(ctx context.Context, ws *Workspace, first int, metrics *SearchMetrics) error {
	for len(ws.fallback) < len(plan.stages) {
		ws.fallback = append(ws.fallback, nil)
	}
	for n := first; n < len(plan.stages); n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		st := &plan.stages[n]
		if !st.canFilter() {
			var started time.Time
			if metrics != nil {
				metrics.reach(n, StageModeBufferedFilter)
				started = time.Now()
			}
			var err error
			ws.fallback[n], err = collectStage(ws.fallback[n][:0], ws, st)
			if metrics != nil {
				stage := &metrics.Stages[n]
				stage.Collected += uint64(len(ws.fallback[n]))
				stage.Duration += time.Since(started)
			}
			if err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (plan Plan) probe(ctx context.Context, candidates []ImageID, ws *Workspace, first int, metrics *SearchMetrics) ([]ImageID, error) {
	for n := first; n < len(plan.stages) && len(candidates) != 0; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st := &plan.stages[n]
		var started time.Time
		if metrics != nil {
			mode := StageModeFilter
			if !st.canFilter() {
				mode = StageModeBufferedFilter
			}
			stage := metrics.reach(n, mode)
			stage.Input += uint64(len(candidates))
			started = time.Now()
		}
		var err error
		switch st.kind {
		case StageNamespace:
			if st.namespaceKind == namespaceRange {
				candidates, err = st.namespaceIndex.FilterRange(candidates[:0], candidates, st.namespaceRange)
			} else {
				candidates, err = st.namespaceQuery.Filter(candidates[:0], candidates)
			}
		case StageGeo:
			candidates, err = st.geoIndex.FilterGeoRanges(candidates[:0], candidates, st.geoRanges)
		case StageACL:
			candidates, err = st.filterACL(candidates[:0], candidates, ws)
		case StageHash:
			out := candidates[:0]
			fallback := ws.fallback[n]
			for _, id := range candidates {
				if _, ok := slices.BinarySearch(fallback, id); ok {
					out = append(out, id)
				}
			}
			candidates = out
		case StageTimestamp:
			out := candidates[:0]
			for _, id := range candidates {
				if st.tsExact != nil {
					stamp, ok := st.tsExact(id)
					if ok && stamp >= st.tsStart && stamp < st.tsEnd {
						out = append(out, id)
					}
				} else if _, ok := slices.BinarySearch(ws.fallback[n], id); ok {
					out = append(out, id)
				}
			}
			candidates = out
		}
		if metrics != nil {
			stage := &metrics.Stages[n]
			stage.Passed += uint64(len(candidates))
			stage.Duration += time.Since(started)
		}
		if err != nil {
			return nil, err
		}
	}
	return candidates, ctx.Err()
}
