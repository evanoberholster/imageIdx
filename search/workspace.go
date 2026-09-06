package search

import imageidx "github.com/evanoberholster/imageIdx"

type ImageID = imageidx.ImageID

// Workspace retains intermediate candidates between searches. A Workspace is
// reusable but must not be used by concurrent searches.
type Workspace struct {
	scratch []ImageID
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
	}
}

// Len returns the number of buffered intermediate IDs.
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
