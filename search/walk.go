package search

import (
	"context"
	"slices"
	"time"

	"github.com/evanoberholster/imageIdx/roar"
)

// bitmap returns an owned/read-safe bitmap. No mmap lifetime escapes an index
// call; a source cursor can safely survive concurrent checkpoint or close.
func (s *stage) bitmap() (roar.Bitmap, error) {
	if s.kind == StageACL {
		return s.aclBitmap()
	}
	if s.kind == StageGeo {
		mutable := roar.NewMutableBitmap()
		s.geoIndex.CollectRanges(mutable, s.geoRanges)
		return mutable.Freeze(), nil
	}
	if s.namespaceKind == namespaceRange {
		return s.namespaceIndex.Range(s.namespaceRange)
	}
	if s.namespaceKind == namespaceBSIRange {
		return s.namespaceIndex.BSIRangeQuery(s.namespaceBSIRange)
	}
	return s.namespaceQuery.Match()
}

// walk emits fully verified, ascending batches. Bitmap sources seek by ID;
// timestamp sources buffer/sort because timeidx has no ID-ordered cursor.
// Probe stages observe live views per batch, not a transaction-wide snapshot.
// The callback may not reuse ws or retain its borrowed batch.
func (plan Plan) walk(ctx context.Context, ws *Workspace, after ImageID, batchSize int, yield func([]ImageID) bool) error {
	return plan.walkWithMetrics(ctx, ws, after, batchSize, yield, nil)
}

func (plan Plan) walkWithMetrics(ctx context.Context, ws *Workspace, after ImageID, batchSize int, yield func([]ImageID) bool, metrics *SearchMetrics) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.IsEmpty() || after == ^ImageID(0) {
		return nil
	}
	if ws == nil {
		ws = &Workspace{}
	}
	if err := plan.prepareProbes(ctx, ws, 1, metrics); err != nil {
		return err
	}
	var cursor roar.Cursor
	var buffered []ImageID
	st := &plan.stages[0]
	var stageStart time.Time
	if metrics != nil {
		metrics.reach(0, StageModeSource)
		stageStart = time.Now()
	}
	if st.kind == StageTimestamp {
		var err error
		ws.source, err = collectStage(ws.source[:0], ws, st)
		if metrics != nil {
			source := &metrics.Stages[0]
			source.Collected = uint64(len(ws.source))
			source.Duration += time.Since(stageStart)
		}
		if err != nil {
			return err
		}
		pos, found := slices.BinarySearch(ws.source, after)
		if found {
			pos++
		}
		buffered = ws.source[pos:]
	} else {
		bit, err := st.bitmap()
		if err != nil {
			if metrics != nil {
				metrics.Stages[0].Duration += time.Since(stageStart)
			}
			return err
		}
		if metrics != nil {
			source := &metrics.Stages[0]
			source.Collected = bit.Cardinality()
			source.Duration += time.Since(stageStart)
		}
		cursor = bit.NewCursor()
	}
	batchSize = min(max(batchSize, 1), executionBatch)
	next := after + 1
	done := false
	for !done {
		if err := ctx.Err(); err != nil {
			return err
		}
		ws.batch = ws.batch[:0]
		if metrics != nil {
			stageStart = time.Now()
		}
		if st.kind == StageTimestamp {
			n := min(batchSize, len(buffered))
			ws.batch = append(ws.batch, buffered[:n]...)
			buffered = buffered[n:]
			done = len(buffered) == 0
		} else {
			for len(ws.batch) < batchSize {
				id, ok := cursor.NextGeq(next)
				if !ok {
					done = true
					break
				}
				ws.batch = append(ws.batch, id)
				if id == ^ImageID(0) {
					done = true
					break
				}
				next = id + 1
			}
		}
		if metrics != nil {
			source := &metrics.Stages[0]
			source.Passed += uint64(len(ws.batch))
			source.Duration += time.Since(stageStart)
		}
		matched, err := plan.probe(ctx, ws.batch, ws, 1, metrics)
		if err != nil {
			return err
		}
		if len(matched) != 0 && !yield(matched) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// Exists reports whether at least one fully verified match exists. It does
// not allocate an ID result set. Buffered timestamp fallbacks still apply.
func (plan Plan) Exists(ctx context.Context, ws *Workspace) (bool, error) {
	found := false
	err := plan.walk(ctx, ws, 0, 1, func([]ImageID) bool { found = true; return false })
	if err != nil {
		return false, err
	}
	return found, nil
}

// countBitmaps uses the N-way cardinality kernel without an ID result slice.
// Plans without a geo stage reuse the workspace bitmap slice and allocate
// nothing. Geo stages are folded as direct bitmap filters: the other bitmap
// stages form the candidate set, and geography filters it in native S2 order
// without materializing its own bitmap.
func (plan Plan) countBitmaps(ctx context.Context, ws *Workspace) (uint64, error) {
	if ws == nil {
		ws = &Workspace{}
	}
	geoPresent := false
	for n := range plan.stages {
		if plan.stages[n].kind == StageGeo {
			geoPresent = true
			break
		}
	}
	if !geoPresent {
		ws.bitmaps = ws.bitmaps[:0]
		defer func() { clear(ws.bitmaps); ws.bitmaps = ws.bitmaps[:0] }()
		for n := range plan.stages {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			bit, err := plan.stages[n].bitmap()
			if err != nil {
				return 0, err
			}
			if bit.IsEmpty() {
				return 0, nil
			}
			ws.bitmaps = append(ws.bitmaps, bit)
		}
		count := roar.AndManyCardinality(ws.bitmaps...)
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return count, nil
	}

	var nonGeo []roar.Bitmap
	var geoStages []*stage
	for n := range plan.stages {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		st := &plan.stages[n]
		if st.kind == StageGeo {
			geoStages = append(geoStages, st)
			continue
		}
		bit, err := st.bitmap()
		if err != nil {
			return 0, err
		}
		if bit.IsEmpty() {
			return 0, nil
		}
		nonGeo = append(nonGeo, bit)
	}
	var candidates roar.Bitmap
	if len(nonGeo) != 0 {
		candidates = roar.AndMany(nonGeo...)
		if candidates.IsEmpty() {
			return 0, nil
		}
	}
	if len(geoStages) == 1 {
		st := geoStages[0]
		m := ws.mutableBitmap()
		m.Reset()
		if len(nonGeo) == 0 {
			st.geoIndex.CollectRanges(m, st.geoRanges)
		} else {
			st.geoIndex.FilterRanges(m, candidates, st.geoRanges)
		}
		return m.Cardinality(), ctx.Err()
	}
	// Multiple geo stages: fold sequentially. Each frozen bitmap is independent
	// because every stage uses a fresh mutable accumulator.
	for n, st := range geoStages {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		m := roar.NewMutableBitmap()
		if n == 0 && len(nonGeo) == 0 {
			st.geoIndex.CollectRanges(m, st.geoRanges)
		} else {
			st.geoIndex.FilterRanges(m, candidates, st.geoRanges)
		}
		candidates = m.Freeze()
		if candidates.IsEmpty() {
			return 0, nil
		}
	}
	return candidates.Cardinality(), ctx.Err()
}
