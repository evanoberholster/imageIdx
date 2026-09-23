package search

import (
	"context"
	"errors"
)

var ErrInvalidOptions = errors.New("search: invalid execution options")

// Page selects up to Limit sorted IDs strictly greater than AfterID. Limit
// must be positive; AppendIDs is the unlimited operation. Pages use live
// views, so writes between calls can change later pages; this is not a snapshot.
type Page struct {
	AfterID ImageID
	Limit   int
}

// AppendIDs appends sorted, unique matches, preserving dst's prefix. On error
// the returned slice has its original length; appended backing storage may
// have changed. A workspace and adequately sized dst avoid scratch growth.
func (plan Plan) AppendIDs(ctx context.Context, dst []ImageID, ws *Workspace) ([]ImageID, error) {
	start := len(dst)
	if err := ctx.Err(); err != nil {
		return dst, err
	}
	out, err := plan.searchInto(ctx, dst, ws, nil)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return out[:start], err
	}
	return out, nil
}

// AppendPage stops after Limit fully verified matches. Bitmap sources seek
// directly after AfterID; timestamp sources require a sorted buffer first.
func (plan Plan) AppendPage(ctx context.Context, dst []ImageID, ws *Workspace, page Page) ([]ImageID, error) {
	if page.Limit <= 0 {
		return dst, ErrInvalidOptions
	}
	start := len(dst)
	err := plan.walk(ctx, ws, page.AfterID, min(page.Limit, executionBatch), func(ids []ImageID) bool {
		remaining := page.Limit - (len(dst) - start)
		dst = append(dst, ids[:min(remaining, len(ids))]...)
		return len(dst)-start < page.Limit
	})
	if err != nil {
		return dst[:start], err
	}
	return dst, nil
}

// Visit invokes yield for sorted, unique matches and stops when it returns
// false. Cancellation is reported rather than disguised as exhaustion. The
// workspace is exclusive until return; yield must not reuse it. Bitmap
// sources use detached snapshots; no mmap reader is pinned across callbacks.
func (plan Plan) Visit(ctx context.Context, ws *Workspace, yield func(ImageID) bool) error {
	if yield == nil {
		return ErrInvalidOptions
	}
	return plan.walk(ctx, ws, 0, executionBatch, func(ids []ImageID) bool {
		for _, id := range ids {
			if ctx.Err() != nil || !yield(id) {
				return false
			}
		}
		return true
	})
}
