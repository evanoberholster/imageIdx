package search

import (
	"context"

	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// VectorResult is one ANN/filtered-vector result. Distance is the raw index
// metric distance (smaller is better); Score is vectoridx's encoded score.
type VectorResult struct {
	ImageID  ImageID
	Distance float32
	Score    uint32
}

// VectorOptions tunes one vector ranking execution. Zero values use the index
// configuration. A negative ExactCutoff disables the exact candidate path.
type VectorOptions struct {
	EfSearch    uint32
	Oversample  uint32
	ExactCutoff int
}

// RankVector applies the plan to an ANN query and appends the best k matches.
// Selective plans (estimate at or below the exact cutoff) collect matching
// image IDs and score them exactly; broader plans run ANN and over-fetch,
// filtering results through the plan until k are found or the index is
// exhausted. The plan's liveness and ACL stages are always applied. Resolve
// idx through Engine.Vector so the namespace belongs to the same unified store
// as the plan.
func (plan Plan) RankVector(ctx context.Context, dst []VectorResult, ws *Workspace, idx *vectoridx.VectorIdx, query []float32, k int) ([]VectorResult, error) {
	return plan.RankVectorWithOptions(ctx, dst, ws, idx, query, k, VectorOptions{})
}

// RankVectorWithOptions is RankVector with per-query controls.
func (plan Plan) RankVectorWithOptions(ctx context.Context, dst []VectorResult, ws *Workspace, idx *vectoridx.VectorIdx, query []float32, k int, opts VectorOptions) ([]VectorResult, error) {
	if idx == nil || k <= 0 {
		return dst, ErrInvalidOptions
	}
	if err := ctx.Err(); err != nil {
		return dst, err
	}
	if plan.IsEmpty() {
		return dst, nil
	}
	if ws == nil {
		ws = &Workspace{}
	}
	pinned, release, pinErr := plan.beginQuery(ctx)
	if pinErr != nil {
		return dst, pinErr
	}
	defer release()
	plan = pinned

	snap := idx.Snapshot()
	if snap == nil {
		return dst, ErrInvalidOptions
	}
	defer snap.Release()
	if err := snap.Err(); err != nil {
		return dst, err
	}

	cutoff := opts.ExactCutoff
	if cutoff == 0 {
		cutoff = snap.Config().FlatSearchCutoff
	}
	estimate := plan.Estimate()
	if cutoff >= 0 && estimate > 0 && estimate <= uint64(cutoff) {
		ids, err := plan.searchInto(ctx, ws.vectorIDs[:0], ws, nil)
		ws.vectorIDs = ids
		if err != nil {
			return dst, err
		}
		if len(ids) == 0 {
			return dst, nil
		}
		if len(ids) <= cutoff {
			ws.vectorNodes = ws.vectorNodes[:0]
			for _, id := range ids {
				if id > ImageID(^uint32(0)) {
					continue
				}
				ws.vectorNodes = append(ws.vectorNodes, vectoridx.NodeID(id))
			}
			results, err := snap.SearchCandidatesIntoFloat32(ctx, ws.vectorResults[:0], ws.vectorNodes, query, k)
			ws.vectorResults = results
			if err != nil {
				return dst, err
			}
			for _, r := range results {
				dst = append(dst, VectorResult{ImageID: ImageID(r.ValueID), Distance: r.Distance, Score: r.Score})
			}
			return dst, nil
		}
	}

	live := snap.LiveCount()
	if live == 0 {
		return dst, nil
	}
	factor := int(snap.Config().OversampleFactor)
	if opts.Oversample != 0 {
		factor = int(opts.Oversample)
	}
	if factor < 1 {
		factor = 1
	}
	over := k * int(factor)
	if over < k {
		over = k
	}
	if over > live {
		over = live
	}
	start := len(dst)
	for {
		ann, err := snap.SearchIntoFloat32WithOptions(ctx, ws.vectorResults[:0], query, vectoridx.SearchOptions{
			K:          over,
			FilterHint: vectoridx.UnknownFilterHint,
			EfSearch:   opts.EfSearch,
			Oversample: opts.Oversample,
		})
		ws.vectorResults = ann
		if err != nil {
			return dst[:start], err
		}
		ids := ws.vectorIDs[:0]
		for _, r := range ann {
			ids = append(ids, ImageID(r.ValueID))
		}
		ws.vectorIDs = ids
		kept, err := plan.filterIDsPinned(ctx, ws.scratch[:0], ids, ws, nil)
		ws.scratch = kept
		if err != nil {
			return dst[:start], err
		}
		dst = appendFilteredVectorResults(dst[:start], ann, kept)
		if len(dst)-start > k {
			dst = dst[:start+k]
		}
		if len(dst)-start >= k || over >= live {
			return dst, nil
		}
		if err := ctx.Err(); err != nil {
			return dst[:start], err
		}
		if over > live/2 {
			over = live
		} else {
			over *= 2
		}
	}
}

// appendFilteredVectorResults preserves the ANN order while keeping only IDs
// the plan accepted. Both slices are distance-ordered; kept is a subsequence.
func appendFilteredVectorResults(dst []VectorResult, ann []vectoridx.Result, kept []ImageID) []VectorResult {
	read := 0
	for _, id := range kept {
		for read < len(ann) && ImageID(ann[read].ValueID) != id {
			read++
		}
		if read == len(ann) {
			break
		}
		dst = append(dst, VectorResult{ImageID: id, Distance: ann[read].Distance, Score: ann[read].Score})
		read++
	}
	return dst
}
