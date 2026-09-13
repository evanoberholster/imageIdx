package search

import (
	"context"
	"sync"

	"github.com/evanoberholster/imageIdx/timeidx"
)

// Service is a concurrency-safe front door for one compiled Plan.
//
// A Plan is immutable after Compile, and index reads register internally
// through each index's ReaderTracker, so any number of goroutines may query
// concurrently provided each execution has a private Workspace and result
// buffer. Service enforces that by pooling workspaces: agents share the
// Service, never a Workspace.
//
// Writes need no service-level lock: bitmapidx indexes serialize
// writers on their internal writerMu, and timeidx.Apply funnels batches
// through its single-owner writerLoop. The one rule writers must follow is
// timestamp's: one Batch per writer at a time. AcquireBatch/ReleaseBatch
// pool reusable batches so writer agents don't allocate per round.
type Service struct {
	plan Plan

	workspaces   sync.Pool
	batches      sync.Pool
	workspaceCap int
	batchCap     int
}

// NewService returns a Service sharing plan across agents. workspaceCap
// sizes pooled scratch; non-positive values default to the plan estimate
// clamped to [64, 65536]. batchCap sizes pooled timestamp batches.
func NewService(plan Plan, workspaceCap, batchCap int) *Service {
	if workspaceCap <= 0 {
		workspaceCap = int(plan.Estimate())
	}
	workspaceCap = min(max(workspaceCap, 64), 65536)
	if batchCap <= 0 {
		batchCap = 64
	}
	service := &Service{plan: plan, workspaceCap: workspaceCap, batchCap: batchCap}
	service.workspaces.New = func() any { return NewWorkspace(workspaceCap) }
	service.batches.New = func() any { return timeidx.NewBatch(batchCap) }
	return service
}

// Estimate returns the shared plan's smallest stage estimate.
func (service *Service) Estimate() uint64 {
	return service.plan.Estimate()
}

// Prewarm places n ready workspaces into the pool so the first queries
// never pay scratch growth under thundering-herd startup. It is a
// best-effort hint: sync.Pool may still drop entries on GC.
func (service *Service) Prewarm(n int) {
	for i := 0; i < n; i++ {
		service.workspaces.Put(NewWorkspace(service.workspaceCap))
	}
}

// IsEmpty reports whether the shared plan matches nothing.
func (service *Service) IsEmpty() bool {
	return service.plan.IsEmpty()
}

// Query appends the plan's sorted, unique intersection to dst. It is safe
// for concurrent use; dst must be agent-private.
func (service *Service) Query(dst []ImageID) []ImageID {
	out, _ := service.QueryContext(context.Background(), dst)
	return out
}

// QueryContext is Query with cancellation checked between stages.
func (service *Service) QueryContext(ctx context.Context, dst []ImageID) ([]ImageID, error) {
	workspace, ok := service.workspaces.Get().(*Workspace)
	if !ok || workspace == nil {
		workspace = NewWorkspace(64)
	}
	out, err := service.plan.SearchContext(ctx, dst, workspace)
	workspace.Reset()
	service.workspaces.Put(workspace)
	return out, err
}

// Count returns the number of matches without retaining them.
func (service *Service) Count() int {
	workspace, ok := service.workspaces.Get().(*Workspace)
	if !ok || workspace == nil {
		workspace = NewWorkspace(64)
	}
	count := service.plan.Count(workspace)
	workspace.Reset()
	service.workspaces.Put(workspace)
	return count
}

// AcquireBatch checks out an exclusive timestamp batch sized for batchCap
// mutations. A checked-out batch must not be shared; return it with Release.
func (service *Service) AcquireBatch() *timeidx.Batch {
	batch, ok := service.batches.Get().(*timeidx.Batch)
	if !ok || batch == nil {
		return timeidx.NewBatch(service.batchCap)
	}
	return batch
}

// ReleaseBatch returns a batch to the pool. Batches still in flight (their
// Apply never completed) are dropped instead of reused.
func (service *Service) ReleaseBatch(batch *timeidx.Batch) {
	if batch == nil {
		return
	}
	if !batch.Reset() {
		return
	}
	service.batches.Put(batch)
}
