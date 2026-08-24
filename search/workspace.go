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
