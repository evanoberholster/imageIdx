package search

import (
	"context"
	"sync"
)

// ExecutorConfig controls query scratch, not index writes or query meaning.
// MaxRetainedBytes is a per-workspace pool budget, not a process memory limit.
type ExecutorConfig struct {
	WorkspaceCapacity int
	MaxRetainedBytes  int
}

// Executor pools private workspaces across plans. It is safe for concurrent
// use; caller result buffers remain caller-owned. It owns no index lifetimes.
type Executor struct {
	pool   sync.Pool
	config ExecutorConfig
}

func NewExecutor(config ExecutorConfig) (*Executor, error) {
	if config.WorkspaceCapacity < 0 || config.MaxRetainedBytes < 0 {
		return nil, ErrInvalidOptions
	}
	if config.MaxRetainedBytes == 0 {
		config.MaxRetainedBytes = 8 << 20
	}
	if config.WorkspaceCapacity == 0 {
		config.WorkspaceCapacity = executionBatch
	}
	config.WorkspaceCapacity = min(config.WorkspaceCapacity, config.MaxRetainedBytes/8)
	e := &Executor{config: config}
	e.pool.New = func() any { return NewWorkspace(config.WorkspaceCapacity) }
	return e, nil
}

func (e *Executor) acquire() *Workspace   { return e.pool.Get().(*Workspace) }
func (e *Executor) release(ws *Workspace) { ws.Trim(e.config.MaxRetainedBytes); e.pool.Put(ws) }

// Prewarm is best effort; sync.Pool may discard entries at any GC.
func (e *Executor) Prewarm(n int) {
	for range max(n, 0) {
		ws := NewWorkspace(e.config.WorkspaceCapacity)
		e.release(ws)
	}
}

func (e *Executor) AppendIDs(ctx context.Context, dst []ImageID, plan Plan) ([]ImageID, error) {
	ws := e.acquire()
	defer e.release(ws)
	return plan.AppendIDs(ctx, dst, ws)
}

// Search executes the plain, uninstrumented bulk search with pooled scratch.
func (e *Executor) Search(ctx context.Context, dst []ImageID, plan Plan) ([]ImageID, error) {
	return e.AppendIDs(ctx, dst, plan)
}

// SearchWithMetrics executes a measured bulk search with pooled scratch.
func (e *Executor) SearchWithMetrics(ctx context.Context, dst []ImageID, plan Plan, metrics *SearchMetrics) ([]ImageID, error) {
	if metrics == nil {
		return dst, ErrInvalidOptions
	}
	ws := e.acquire()
	defer e.release(ws)
	return plan.SearchWithMetrics(ctx, dst, ws, metrics)
}
func (e *Executor) AppendPage(ctx context.Context, dst []ImageID, plan Plan, page Page) ([]ImageID, error) {
	ws := e.acquire()
	defer e.release(ws)
	return plan.AppendPage(ctx, dst, ws, page)
}
func (e *Executor) FilterIDs(ctx context.Context, dst, candidates []ImageID, plan Plan) ([]ImageID, error) {
	ws := e.acquire()
	defer e.release(ws)
	return plan.FilterIDs(ctx, dst, candidates, ws)
}

// FilterIDsWithMetrics executes a measured candidate filter with pooled scratch.
func (e *Executor) FilterIDsWithMetrics(ctx context.Context, dst, candidates []ImageID, plan Plan, metrics *SearchMetrics) ([]ImageID, error) {
	if metrics == nil {
		return dst, ErrInvalidOptions
	}
	ws := e.acquire()
	defer e.release(ws)
	return plan.FilterIDsWithMetrics(ctx, dst, candidates, ws, metrics)
}
func (e *Executor) Count(ctx context.Context, plan Plan) (uint64, error) {
	ws := e.acquire()
	defer e.release(ws)
	return plan.Count(ctx, ws)
}
func (e *Executor) Exists(ctx context.Context, plan Plan) (bool, error) {
	ws := e.acquire()
	defer e.release(ws)
	return plan.Exists(ctx, ws)
}
