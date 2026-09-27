package search

import (
	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
	"unsafe"
)

type ImageID = common.ImageID

// Workspace retains intermediate candidates between searches. A Workspace is
// reusable but must not be used by concurrent or reentrant searches. It owns
// reusable candidate and bitmap scratch. Allocations depend on index deltas,
// mmap result detachment and required fallback buffers; reuse avoids growth. The
// zero value is valid: buffers grow on first use exactly as a sized
// workspace would.
type Workspace struct {
	scratch []ImageID
	mutable *roar.MutableBitmap
	// fallback holds sorted timestamp candidates for stages without a probe API.
	fallback   [][]ImageID
	batch      []ImageID
	source     []ImageID
	bitmaps    []roar.Bitmap
	aclMask    []bool
	aclMatches []ImageID
}

func NewWorkspace(capacity int) *Workspace {
	if capacity < 0 {
		capacity = 0
	}
	return &Workspace{scratch: make([]ImageID, 0, capacity)}
}

// Reset clears temporary results while retaining allocated buffer capacity.
func (workspace *Workspace) Reset() {
	if workspace != nil {
		workspace.scratch = workspace.scratch[:0]
		workspace.batch = workspace.batch[:0]
		workspace.source = workspace.source[:0]
		workspace.aclMask = workspace.aclMask[:0]
		workspace.aclMatches = workspace.aclMatches[:0]
		clear(workspace.bitmaps)
		workspace.bitmaps = workspace.bitmaps[:0]
		for n := range workspace.fallback {
			workspace.fallback[n] = workspace.fallback[n][:0]
		}
		if workspace.mutable != nil {
			workspace.mutable.Reset()
		}
	}
}

// RetainedBytes conservatively estimates scratch retained between calls.
// Bitmap payloads are released by Reset; mutable scratch may be over-counted.
func (workspace *Workspace) RetainedBytes() int {
	if workspace == nil {
		return 0
	}
	n := int(unsafe.Sizeof(*workspace)) + 8*(cap(workspace.scratch)+cap(workspace.batch)+cap(workspace.source)+cap(workspace.aclMatches)) + cap(workspace.aclMask) +
		cap(workspace.bitmaps)*int(unsafe.Sizeof(roar.Bitmap{})) + cap(workspace.fallback)*int(unsafe.Sizeof([]ImageID{}))
	for _, ids := range workspace.fallback {
		n += 8 * cap(ids)
	}
	return n + workspace.mutable.RetainedBytes()
}

// Trim resets scratch and drops all buffers if they exceed maxBytes. A zero
// budget releases all buffers. Do not call while an execution is active.
func (workspace *Workspace) Trim(maxBytes int) {
	if workspace == nil {
		return
	}
	workspace.Reset()
	if maxBytes <= 0 || workspace.RetainedBytes() > maxBytes {
		*workspace = Workspace{}
	}
}

// mutableBitmap returns the workspace-owned transient set accumulator,
// allocating it on first use. Callers must Reset it before accumulating;
// Workspace.Reset clears it for the next pooled query.
func (workspace *Workspace) mutableBitmap() *roar.MutableBitmap {
	if workspace == nil {
		return roar.NewMutableBitmap()
	}
	if workspace.mutable == nil {
		workspace.mutable = roar.NewMutableBitmap()
	}
	return workspace.mutable
}

// Len returns the number of buffered intermediate IDs in scratch. Source,
// fallback and batch buffers are excluded; use RetainedBytes for pool sizing.
func (workspace *Workspace) Len() int {
	if workspace == nil {
		return 0
	}
	return len(workspace.scratch)
}

// Cap returns the scratch buffer capacity.
func (workspace *Workspace) Cap() int {
	if workspace == nil {
		return 0
	}
	return cap(workspace.scratch)
}

// EnsureCapacity grows scratch to hold at least n IDs without losing
// retained capacity. It allocates at most once.
func (workspace *Workspace) EnsureCapacity(n int) {
	if workspace == nil || n <= cap(workspace.scratch) {
		return
	}
	grown := make([]ImageID, len(workspace.scratch), n)
	copy(grown, workspace.scratch)
	workspace.scratch = grown
}
